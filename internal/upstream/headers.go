// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/wbid"
)

const (
	clientUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	defaultClientVer    = "5.5.4"
)

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return originRefererGlobal
	}
	return originRefererCN
}

// CommonHeaders 设置所有 API 共享的请求头。
func CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	injectAccountStableHeaders(req, a)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
//
// conv / turn 是 wbid.Anchors 抽出的会话锚点（国内版此前完全不发 X-Conversation-ID，
// 属单边裸奔 —— 见 R116）；锚点为空时退回每请求随机，行为与历史一致。
func ChatHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	injectAttribution(req, a)
	injectConversationHeaders(req, a, conv, turn)
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// **调用方必须持有 a.Lock()**（本函数直接读 a.RefreshToken，不加锁——
// 若调用方未持锁，与 keepalive 刷新写回构成数据竞争）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// injectAttribution 注入用量归属头——使上游调用记录显示为 WorkBuddy 桌面端。
func injectAttribution(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", defaultClientVer)
	req.Header.Set("X-Product", "WorkBuddy")
}

// injectConversationHeaders 注入会话头族（X-Conversation-* / B3 链路）。
//
// 语义与形态见 internal/wbid：会话级恒定、轮级一次 user send（工具回调/重试复用）、
// 调用级每请求新生成；X-Root-Request-ID 取本轮 id（官方主对话同形）。
// B3 是每请求链路头，仍跟调用级 id 走。
func injectConversationHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	msgID := wbid.MessageID()
	turnID := wbid.TurnID(uid, conv, turn)
	req.Header.Set("X-Conversation-ID", wbid.ConversationID(uid, conv))
	req.Header.Set("X-Conversation-Request-ID", turnID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", turnID)
	req.Header.Set("X-B3-TraceId", msgID)
	req.Header.Set("X-B3-SpanId", msgID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return "en-US"
	}
	return "zh-CN"
}

// injectAccountStableHeaders 注入按 uid 稳定派生的 X-Machine-ID / X-Session-ID（36 hex）。
func injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveStableID(a.UID, "session"))
}

func deriveStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}
