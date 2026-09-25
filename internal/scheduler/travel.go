// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// activityReportGap 同一账号内连续上报之间的间隔：5 连发模拟同一会话多轮对话，
// 秒发易触发风控，故 1.5s 一条。测试可置 0。
var activityReportGap = 1500 * time.Millisecond

// checkinAccountDelay 签到账号间限速。此前签到是唯一**没有**账号间距的任务族：
// 旅行与活跃上报都有限速，签到却是一个 for 循环连着打 /daily-checkin——N 个账号在
// 几百毫秒内密集签到，是这批账号"同一台机器/同一批脚本"最直接的旁证。
// 签到是最敏感的操作（涉及账号权益），间距取值比其余任务族更大。测试可置 0。
var checkinAccountDelay = 1200 * time.Millisecond

// sleepCtx 可取消的等待：ctx 取消立即返回 false（优雅停机不必等 sleep 醒来），
// 等满返回 true。d<=0 立即放行（测试把延迟置 0 时不白等）。
// 替换 time.Sleep：账号间/账号内限速值不变，只换等待方式。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pacedSleep 抖动后的限速等待：在 d 的基础上叠加 ±pacedJitterRatio 的随机量。
//
// 定长间隔是"机器节奏"本身：每个账号精确间隔 800ms、每条上报精确间隔 1.5s，
// 连起来是一条整齐的脉冲串，比随机间隔显眼得多。抖动源用 math/rand/v2 全局源
// （非加密安全，此处只需要打散，不需要不可预测性）。
//
// d<=0 时直接放行，保持既有测试"置 0 即不等待"的语义。
func pacedSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, jitterDuration(d))
}

// pacedJitterRatio 限速抖动幅度（±20%）。
const pacedJitterRatio = 0.2

// jitterDuration 把 d 抖动到 [0.8d, 1.2d] 区间；d<=0 原样返回。
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	// rand.Float64() ∈ [0,1) → 偏移量 ∈ [-0.2d, +0.2d)
	span := float64(d) * pacedJitterRatio
	out := time.Duration(float64(d) + (2*rand.Float64()-1)*span)
	if out <= 0 {
		return d
	}
	return out
}

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检（无 ctx 的外部入口：
// cmd/手动触发、测试）。内部走 runTravel，取背景 ctx（不可取消，语义与
// 引入前 time.Sleep 版一致）。
func (s *Scheduler) RunTravelNow() []Outcome {
	return s.runTravel(context.Background())
}

// runTravel 旅行巡检遍历，随 ctx 取消立即退出；返回逐账号结果。
// 禁用账号跳过；401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay（sleepCtx：取消时立即放弃后续账号）。
func (s *Scheduler) runTravel(ctx context.Context) []Outcome {
	out := []Outcome{}
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 无猫猫旅行体系，不发起任何上游调用
		}
		if !first {
			if !pacedSleep(ctx, travelAccountDelay) {
				return out // 优雅停机：不等限速睡满，剩余账号下轮再巡
			}
		}
		first = false
		out = append(out, s.travelOne(a))
	}
	return out
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
func (s *Scheduler) travelOne(a *auth.Auth) Outcome {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "buddy-info: " + err.Error()}
	}
	if buddy == nil {
		return s.travelAdopt(a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "status: " + err.Error()}
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.UID8(a.UID), ts.RecordID)
		return Outcome{UID: a.UID, OK: true, Status: "skipped", Message: fmt.Sprintf("traveling record=%d", ts.RecordID)}
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.UID8(a.UID), ts.State)
		return Outcome{UID: a.UID, OK: true, Status: StatusSkipped, Message: "旅行状态未知（" + ts.State + "），本轮未处理"}
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) Outcome {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.UID8(a.UID))
		return Outcome{UID: a.UID, OK: true, Status: StatusSkipped, Message: "今日旅行次数已达上限，未再派出"}
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "depart: " + err.Error()}
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.UID8(a.UID), travelLocationID)
	return Outcome{UID: a.UID, OK: true, Status: "ok", Message: fmt.Sprintf("depart location=%d", travelLocationID)}
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) Outcome {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.UID8(a.UID))
		return Outcome{UID: a.UID, OK: false, Status: "skipped", Message: "arrived but no record_id"}
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.UID8(a.UID), ts.RecordID, err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: fmt.Sprintf("claim record=%d: %v", ts.RecordID, err)}
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.UID8(a.UID), ts.RecordID, reward)
	return Outcome{UID: a.UID, OK: true, Status: "ok", Reward: reward, Message: fmt.Sprintf("claim record=%d", ts.RecordID)}
}

// travelAdopt 旅行巡检时领养：受 adoptTriedToday 当日防抖约束。
func (s *Scheduler) travelAdopt(a *auth.Auth) Outcome {
	return s.adoptBuddy(a, false)
}

// travelAdoptForce 活跃上报补满对话量后领养：豁免 adoptTriedToday 当日防抖。
// 背景：旅行排程 09 点已领养且因对话量未达 skip，10 点活跃上报 5 连发把
// 对话量补满——此时是「门槛刚达成」的新状态，不算对上游重试轰炸，放行重试。
// 有猫账号 BuddyInfo 非空时直接跳过（不重复领养）。
func (s *Scheduler) travelAdoptForce(a *auth.Auth) Outcome {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("activity %s: buddy-info: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "buddy-info: " + err.Error()}
	}
	if buddy != nil {
		return Outcome{UID: a.UID, OK: true, Status: "skipped", Message: "already has buddy"}
	}
	return s.adoptBuddy(a, true) // force=true 豁免当日防抖
}

// adoptBuddy 无猫时领养：先同意协议（幂等）再 buddy/first。
// conversation 门槛未达标（HTTP 400 first_buddy task not completed yet）属预期行为，
// 记一次当日已试后静默跳过，不再重试。force=true 时豁免当日防抖（活跃上报补满对话量后重试）。
func (s *Scheduler) adoptBuddy(a *auth.Auth, force bool) Outcome {
	if !force && s.adoptTriedToday(a.UID) {
		return Outcome{UID: a.UID, OK: true, Status: "skipped", Message: "adopt already tried today"}
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "agreement: " + err.Error()}
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.UID8(a.UID))
		return Outcome{UID: a.UID, OK: true, Status: "ok", Reward: 300, Message: "adopt ok"}
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.UID8(a.UID))
		return Outcome{UID: a.UID, OK: true, Status: "skipped", Message: "conversation threshold not reached"}
	default:
		log.Printf("travel %s: adopt: %v", logfmt.UID8(a.UID), err)
		return Outcome{UID: a.UID, OK: false, Status: "fail", Message: "adopt: " + err.Error()}
	}
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
