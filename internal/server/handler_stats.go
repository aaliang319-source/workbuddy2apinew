// handler_stats.go /v1/stats 请求统计端点：面板"请求统计"页的数据源。
// 契约与 workbuddy2api-gui/internal/gateway/client.go 的 Stats/ModelStat 对应。
package server

import (
	"net/http"
	"time"

	"workbuddy2api/internal/metrics"
)

// observeMetrics 请求出口统一上报 /v1/stats 观测。Metrics==nil（统计关闭）时空操作。
// 只在 chatCompletions 出口 defer 调用一次：旋转多账号整个记 1 次（请求级口径）。
func (h *Handler) observeMetrics(st *chatStat, model string, stream bool) {
	if h.cfg.Metrics == nil {
		return
	}
	completion := int64(st.toks)
	if completion < 0 {
		completion = 0 // usage 缺失（toks=-1）：请求仍计入 requests/latency，token 记 0
	}
	h.cfg.Metrics.Observe(metrics.Observe{
		Model:            model,
		Stream:           stream,
		Status:           st.status,
		TTFB:             st.ttfb,
		Wall:             time.Since(st.start),
		PromptTokens:     st.mPrompt,
		CompletionTokens: completion,
		CacheHit:         st.mHit,
		CacheMiss:        st.mMiss,
		CacheWrite:       st.mWrite,
		CacheObserved:    st.mHasCache,
		Credit:           st.mCredit,
		CreditOK:         st.mCreditOK,
		Err:              st.errMsg,
		UID:              uidPrefix(st.uid),
		Proto:            st.proto,
		KeyName:          st.keyName,
		Tries:            st.tries,
		RespModel:        st.respModel,
		Now:              time.Now(),
	})
}

// stats GET /v1/stats：按模型聚合的请求统计。统计关闭（Metrics==nil）时返回
// 200 + enabled=false（面板据此渲染"网关未启用统计"提示，不是错误）。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"message": "metrics disabled: set server.metrics_enabled=true in gateway config",
		})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Metrics.Snapshot())
}

// statsReset POST /v1/stats/reset：清空累计并把统计起点重置为当前时刻。
// body 忽略；面板只看状态码。统计关闭时同样 200（幂等无害）。
func (h *Handler) statsReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Metrics == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "ok": false})
		return
	}
	h.cfg.Metrics.Reset()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
