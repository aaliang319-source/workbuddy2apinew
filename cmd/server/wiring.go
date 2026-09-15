package main

import (
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
//
// mix=true（config global.mix_realms）时忽略 realm 前缀，返回两域混合的可用集合——
// 与选号侧 pickRealm="" 同口径，否则粘性重绑定会把 global 账号挡在 CN 请求之外。
func realmAwareAvailableForModel(p *pool.Pool, mix bool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		if mix {
			realm = ""
		}
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}