// handler_messages.go Anthropic 兼容端点：POST /v1/messages 与 /v1/messages/count_tokens。
//
// 职责边界：本文件只做「协议适配 + 轮转编排」；请求翻译在 anthropic_translate.go，
// 响应翻译（SSE 状态机/非流式映射/错误信封）在 anthropic_sse.go。
// 轮转循环是 chatCompletions（handler.go:386-722）的精简复刻（D1 决策：不做共享抽取，
// 避免给战斗测试过的主路径加 emitter 抽象）：选号/Acquire/token 刷新/错误分派/降级重试
// 逐行对齐，复用 applyErrorPolicy / pool 记账 / chatStatsReader / observeMetrics 原语，
// 唯三差异是错误写出走 writeAnthropicError（Anthropic 信封，503=overloaded_error）。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// messages POST /v1/messages：Anthropic Messages API 主端点。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	// 请求体上限与 413 语义照抄 chatCompletions（issue #41：截断 body 不喂上游）。
	limit := h.cfg.MaxBodyBytes
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(raw)) > limit {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// max_tokens 是 Anthropic 协议强制字段（缺省无默认值语义），缺失直接 400。
	if req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "max_tokens: field required (Anthropic Messages API mandatory field)")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages: at least one message is required")
		return
	}

	// 模型路由：claude-* 等客户端模型名 → 网关模型（前缀直通 / model_map / 已知裸名 / 缺省），
	// 再走标准 resolveModel 得 realm + bareModel（选号/账本/粘性全链路复用）。
	mappedModel := h.resolveAnthropicModel(req.Model)
	realm, bareModel := resolveModel(mappedModel)
	// 混合调度（config global.mix_realms）：选号不按 realm 过滤，与 chatCompletions 同构。
	pickRealm := realm
	if h.cfg.MixRealms {
		pickRealm = ""
	}

	// 翻译为 OpenAI body（system/messages/tools/tool_choice/metadata 全量映射）；
	// 不支持的块类型（image 等）在此 400，不打上游不罚号。
	openaiBodyMap, err := translateAnthropicToOpenAI(&req, mappedModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := json.Marshal(openaiBodyMap)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "marshal translated body: "+err.Error())
		return
	}

	st := newChatStat(time.Now(), body, req.Stream)
	st.proto = "anthropic"  // 单条明细协议来源
	st.reqModel = req.Model // 原始请求模型名（body 已是翻译后的 OpenAI 形态，parse 不到原始名）
	defer st.done()
	servedModel := bareModel // 实际服务模型（模型回退后变化；出口按它记录明细/统计）
	defer func() { h.observeMetrics(st, servedModel, req.Stream) }()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：翻译层已把 Anthropic metadata.user_id 注入为 OpenAI metadata.conversation_id，
	// ExtractKey 零改动识别；无该键时 TurnKey 按最后一条 user 消息派生轮级键（同 OpenAI 客户端语义）。
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 传完整模型名（含 realm 前缀）：粘性命中闭包内部自行剥前缀过滤 realm，见 handler.go:438-448。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, mappedModel); ok {
			stickyUID = uid
		}
	}
	turnKey := ""
	if sessKey == "" {
		turnKey = session.TurnKey(body)
	}

	// 在途租约与失败清理闭包：照抄 chatCompletions（handler.go:460-487）。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写：与 chatCompletions 同策略（custom 替换 / passthrough+降级期切 Degraded）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}
	// 出站 model 重写为裸名（realm 前缀是网关路由协议，上游不认）。
	if bareModel != mappedModel {
		body = rewriteModel(body, bareModel)
	}

	// Key 模型白名单 = 自动路由范围（与 chatCompletions 同构）：仅约束 auto 档
	// 请求与回退序列；用户主动指定的模型原样透传，不受白名单限制。
	var keyModelSeq []string
	if k := handlerKey(r); k != nil && len(k.Models) > 0 {
		for _, m := range k.ModelsSorted() {
			keyModelSeq = append(keyModelSeq, m.Name)
		}
		if bareModel == "auto" && keyModelSeq[0] != "auto" {
			body = rewriteModel(body, keyModelSeq[0])
			bareModel = keyModelSeq[0]
			servedModel = keyModelSeq[0]
			st.model = keyModelSeq[0]
			log.Printf("INFO: [server] key auto-routing: model %q -> %q (key %q whitelist top)",
				"auto", keyModelSeq[0], k.Name)
		}
	}

	// 会话头族：与 chatCompletions 同构（同轮 tool call / 换号 / 降级共享同一 ConversationRequestID）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 响应级 Request-Id（Anthropic 客户端/SDK 习惯读该头做日志关联）。
	reqID := "req_" + randomHex8()

	// Key 关联范围：多 Key 模式下选号限定在该 Key 关联（且启用）的账号子集内，
	// 并按关联优先级分层（PickForKey）；nil = 旧单 Key 模式全池。与 chatCompletions 同构。
	var keyScope *pool.KeyScope
	if k := handlerKey(r); k != nil {
		keyScope = &pool.KeyScope{Allowed: h.cfg.Keys.AllowedUIDs(k.ID)}
	}
	// nextFallbackModel 模型回退链（与 chatCompletions 同构；6004/11102/403/账号冷却均计入"无账号可服务"）。
	fbIdx := 0
	// 回退候选序列：Key 白名单优先，未配置用全局 model_fallback（同构 chatCompletions）。
	fallbackSeq := keyModelSeq
	if len(fallbackSeq) == 0 {
		fallbackSeq = h.cfg.ModelFallback
	}
	nextFallbackModel := func(realm, current string) (string, bool) {
		for fbIdx < len(fallbackSeq) {
			fb := fallbackSeq[fbIdx]
			fbIdx++
			if fb == "" || fb == current {
				continue
			}
			if !h.cfg.Pool.ModelGoneRealm(realm, current, keyScope) {
				return "", false
			}
			return fb, true
		}
		return "", false
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 粘性号不在本 Key 关联集内（他 Key 绑定/关联已变更）时先解绑，避免跨 Key 泄漏流量。
		if stickyUID != "" && keyScope != nil {
			if _, allowed := keyScope.Allowed[stickyUID]; !allowed {
				unbindSticky()
			}
		}
		var acct *auth.Auth
		if stickyUID != "" {
			// Scoped 版：Key 关联外或存在更高优先层可用号时返回 nil → 解绑重选
			//（优先级高于粘性，与 chatCompletions 同构）。
			acct = h.cfg.Pool.PickByUIDForModelScoped(stickyUID, bareModel, pickRealm, keyScope)
			if acct == nil || (pickRealm != "" && acct.Realm() != pickRealm) {
				unbindSticky()
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickForKey(tried, bareModel, pickRealm, keyScope)
		}
		if acct == nil {
			// 模型级故障转移（与 chatCompletions 同构）。
			if fb, ok := nextFallbackModel(pickRealm, bareModel); ok {
				body = rewriteModel(body, fb)
				bareModel = fb
				servedModel = fb
				st.model = fb
				tried = map[string]bool{}
				i--
				continue
			}
			st.status = http.StatusServiceUnavailable
			if st.errMsg == "" {
				st.errMsg = "no_account_available(503)"
			}
			break
		}
		st.uid = acct.UID
		tried[acct.UID] = true

		if !h.cfg.Pool.Acquire(acct.UID) {
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue
		}
		heldUID = acct.UID
		st.tries++ // 真正出站的尝试计数（明细换号次数维度）

		// token 临近过期先刷新（失败按 session-dead 计数/喂熔断后换号）——与主路径同口径。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			acct.BackfillRealm()
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("ERR: [server] messages refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		if terr != nil {
			// 传输层抖动：换号不喂熔断（与主路径一致）。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			st.errMsg = "transport_error(503)"
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			st.status = status
			kind := upstream.Classify(status, string(respBody))
			// 单条请求明细失败原因：只记权威分类 + 状态码（防泄露，见 D 决策）。
			st.errMsg = fmt.Sprintf("%s(%d)", kind, status)
			// 指纹误报降级重试：与主路径同逻辑，仅错误写出形态不同（见下 content_blocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID)
				releaseHeld()
				log.Printf("WARN: [server] messages content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 终态：内容防火墙命中，不轮转不罚号（换号结果相同），Anthropic 400 信封。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, status)
				fail(acct.UID)
				msg := upstream.ContentBlockedClientMessage(string(respBody))
				writeAnthropicError(w, http.StatusBadRequest, msg)
				st.status = http.StatusBadRequest
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, status)
			fail(acct.UID)
			continue
		}

		// 成功：池记账 + 粘性重绑（与主路径一致）。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		w.Header().Set("Request-Id", reqID)

		if req.Stream {
			// 流式：chatStatsReader 采集 TTFB/usage/credit（读的是上游原始 OpenAI SSE，
			// 与 OpenAI 路径完全同源），anthropicStreamTransform 负责协议翻译与 flush。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			_ = anthropicStreamTransform(w, stats, mappedModel)
			st.ttfb = stats.TTFB()
			st.toks, _ = stats.Tokens()
			if m := stats.Model(); m != "" {
				st.respModel = m // 上游回传的实际模型（auto 档路由结果，进明细）
			}
			if p, ok := stats.PromptTokens(); ok {
				st.mPrompt = int64(p)
			}
			st.mHit, st.mMiss, st.mWrite, st.mHasCache = stats.Cache()
			if credit, creditOK := stats.Credit(); creditOK {
				st.mCredit, st.mCreditOK = credit, true
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
			} else if _, hasUsage := stats.Tokens(); hasUsage {
				log.Printf("WARN: [server] messages stream usage without credit uid=%s model=%s (no cost observation)", logfmt.UID8(acct.UID), bareModel)
			}
			rc.Close()
			return
		}
		// 非流式：Aggregate 出 OpenAI 形态再映射为 Anthropic Message 对象。
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "upstream stream contained no valid data events")
			st.status = http.StatusBadGateway
			return
		}
		// 实际模型：Aggregate 输出里取上游回传的 model（auto 档路由结果，进明细）。
		if m, ok := resp["model"].(string); ok && m != "" && st.respModel == "" {
			st.respModel = m
		}
		writeJSON(w, http.StatusOK, anthropicMessageFromOpenAI(resp, mappedModel))
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		if p, ok := usagePromptTokens(resp); ok {
			st.mPrompt = p
		}
		st.mHit, st.mMiss, st.mWrite, st.mHasCache = usageCacheTokens(resp)
		if credit, total, ok := usageCreditTotal(resp); ok {
			st.mCredit, st.mCreditOK = credit, true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}

	// 末端错误规范化：分类映射对齐 chatCompletions 尾部（429 限流语义 / 11101 白名单
	// 透传 / 其余 503），仅信封换成 Anthropic 形态；503 → overloaded_error 促使
	// Claude Code 退避重试。
	status := http.StatusServiceUnavailable
	msg := "all accounts are temporarily unavailable, please retry later"
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrBadParams:
			msg = "upstream rejected request params: " + ue.Msg
		}
	}
	writeAnthropicError(w, status, msg)
	st.status = status
}

// countTokens POST /v1/messages/count_tokens：Claude Code 会调用，但官方客户端对
// 该端点失败有本地估算回退。这里给 len/4 启发式估算（含 system + 每消息 4 token
// 消耗），解析失败也 200（宽容：计数器不该阻断主流程）。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, h.cfg.MaxBodyBytes+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if json.Unmarshal(raw, &req) != nil {
		writeJSON(w, http.StatusOK, map[string]any{"input_tokens": 0})
		return
	}
	total := 0
	if sys := anthropicSystemText(req.System); sys != "" {
		total += utf8Len4(sys)
	}
	for _, m := range req.Messages {
		total += utf8Len4(anthropicContentText(m.Content))
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": total})
}

// utf8Len4 rune 数/4 + 每消息 4 的启发式 token 估算（count_tokens 专用）。
func utf8Len4(s string) int {
	if s == "" {
		return 0
	}
	n := len([]rune(s)) / 4
	if n == 0 {
		n = 1
	}
	return n + 4
}

// resolveAnthropicModel 客户端模型名 → 网关模型名。四级查找：
//  1. cn:/global: 前缀 → 原样直通（网关路由协议，也允许 Anthropic 客户端点名网关模型）；
//  2. anthropic.model_map 精确命中（claude-sonnet-4-5 → cn:glm-5.3 之类）；
//  3. 裸名命中静态已知 CN 模型表 → "cn:"+名（客户端直写网关模型名的宽容路径）；
//  4. 其余（claude-* / 未知）→ anthropic.default_model（缺省 cn:auto）。
func (h *Handler) resolveAnthropicModel(model string) string {
	if model == "" {
		return h.anthropicDefault()
	}
	if strings.HasPrefix(model, "cn:") || strings.HasPrefix(model, "global:") {
		return model
	}
	if mapped, ok := h.cfg.AnthropicModelMap[model]; ok && mapped != "" {
		return mapped
	}
	for _, m := range staticModels {
		if id, ok := m["id"].(string); ok && id == model {
			return "cn:" + model
		}
	}
	// 动态模型识别：上游模型列表里的新模型（glm-5.3-flash 等静态表没有的名字）
	// 同样允许客户端点名——否则会被误映射到 default_model(auto)，用户在客户端
	// 显式选择的网关模型名根本不生效（实测踩坑）。
	for _, mi := range h.fetchDynamicModels() {
		if mi.ID == model {
			return "cn:" + model
		}
	}
	return h.anthropicDefault()
}

func (h *Handler) anthropicDefault() string {
	if h.cfg.AnthropicDefaultModel != "" {
		return h.cfg.AnthropicDefaultModel
	}
	return "cn:auto"
}

// randomHex8 请求级 Request-Id 用 8 字节随机 hex。
func randomHex8() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
