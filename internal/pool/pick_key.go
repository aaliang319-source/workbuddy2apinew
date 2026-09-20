// Key 关联分层选号：PickForKey 在 Key 关联的账号子集内按优先级分层挑选。
// 语义（用户确认）：先只在最高优先级层内选号（层内沿用 pick 的三因子加权随机），
// 该层全部不可用（冷却/占满/在途满）才降级到下一层；全冷却兜底同样限定在关联
// 子集内（不分层，取最早到期者——降级本身就是最后手段，不再叠层）。
// scope == nil 时 PickForKey 等价 pick（零回归）。
package pool

import (
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// KeyScope Key 关联的账号范围：uid → 优先级（越大越优先）。
// nil 指针 = 不限制（退化为全池选号）。
type KeyScope struct {
	Allowed map[string]int
}

// PickForKey 在 scope 限定的账号子集内分层选号。
// 候选过滤与 pick 一致（healthy/healthyForModel、realm、tried、在途占满），
// 叠加 scope.Allowed 存在性过滤；随后按优先级取最高非空层，
// 层内走与 pick 完全相同的加权选号主体（pickFromLocked）。
func (p *Pool) PickForKey(tried map[string]bool, reqModel, realm string, scope *KeyScope) *auth.Auth {
	if scope == nil || len(scope.Allowed) == 0 {
		// nil / 空 scope：等价普通选号（空 scope 由 handler 的 403 拦截，防御性兜底）。
		return p.pick(tried, reqModel, realm)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// 惰性清理过期的 6004 模型级冷却（与 pick 同口径）。
	for _, e := range p.byUID {
		e.pruneExpiredModelCooldowns(now)
	}
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if _, ok := scope.Allowed[uid]; !ok {
			continue // Key 关联过滤：scope 外账号不参与
		}
		if !healthyOf(e) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// 全冷却兜底：限定在关联子集内（禁用/CoolHard 照旧排除）。
		return p.pickEarliestExpiryLocked(tried, now, realm, scope)
	}
	// 分层：只保留最高优先级层（层内账号在该层全灭前不会被低层抢流量）。
	bestPrio := scope.Allowed[cands[0].a.UID]
	for _, e := range cands[1:] {
		if pr := scope.Allowed[e.a.UID]; pr > bestPrio {
			bestPrio = pr
		}
	}
	tier := make([]*entry, 0, len(cands))
	for _, e := range cands {
		if scope.Allowed[e.a.UID] == bestPrio {
			tier = append(tier, e)
		}
	}
	return p.pickFromLocked(tier, reqModel, now)
}

// PickByUIDForModelScoped 粘性命中校验（Key 感知版）：在 PickByUIDForModel 的
// 账号级/模型级可用性校验之上叠加 Key 关联与优先级约束——
//   - uid 不在 Key 关联集内 → nil（handler 解绑，避免跨 Key 泄漏流量）；
//   - 关联集内存在**严格更高优先级**且当前可用的账号 → nil（handler 解绑后由
//     PickForKey 按层重选）。没有这一步，粘性会把同一会话永久钉在低优先层：
//     绑定一旦落在 prio 6 的号上，prio 10/9 的号再健康也永远拿不到流量，
//     优先级形同虚设（用户实测：全部请求都打在最低优先级账号）。
// scope 为 nil/空（旧单 Key 模式或通配 Key）时退化为 PickByUIDForModel。
func (p *Pool) PickByUIDForModelScoped(uid, model, realm string, scope *KeyScope) *auth.Auth {
	if scope == nil || len(scope.Allowed) == 0 {
		return p.PickByUIDForModel(uid, model)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	myPrio, allowed := scope.Allowed[uid]
	if !allowed {
		return nil
	}
	now := time.Now()
	// 更高优先层是否有当前可用账号（realm + healthyForModel + 在途未满，与选号同口径）。
	for guid, g := range p.byUID {
		if guid == uid || scope.Allowed[guid] <= myPrio {
			continue
		}
		if realm != "" && g.a.Realm() != realm {
			continue
		}
		if !g.healthyForModel(now, model) || p.inFlightFull(g) {
			continue
		}
		return nil // 更高优先层可用：交 PickForKey 按层重选（优先级 > 粘性）
	}
	if !e.healthyForModel(now, model) || p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	return e.a
}

// ModelGoneRealm 报告"该模型已被 realm 域内所有可用账号拒绝"——切模型回退的触发信号。
// 判定口径：域内每个非禁用账号，要么对该模型有活跃的拒绝负缓存（11102 无此模型 /
// 403 WAF 拒绝），要么账号本身不健康（账号级冷却/熔断——无法证明它有该模型）。
// 只要还有"健康且未拒绝过该模型"的账号，就不算模型没了（轮换还会试它）。
// 6004 模型级限流不算"模型没了"（会自动重置，等冷却而非切模型）。
// realm==""（混合调度）统计全池。调用方不持锁。
func (p *Pool) ModelGoneRealm(realm, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	blocked, healthy := 0, 0
	for _, e := range p.byUID {
		if e.disabled || (realm != "" && e.a.Realm() != realm) {
			continue
		}
		if mc, ok := e.modelCooldowns[model]; ok && now.Before(mc.Until) {
			if strings.HasPrefix(mc.Reason, "11102") || strings.HasPrefix(mc.Reason, "403") {
				blocked++
			}
			continue
		}
		if e.healthy(now) {
			healthy++
		}
	}
	return blocked > 0 && healthy == 0
}
