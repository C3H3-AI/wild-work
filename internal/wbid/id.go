// Package wbid 生成 WorkBuddy 会话头族（X-Conversation-*）要的 id。
//
// 两个区（国内版 copilot.tencent.com / 国际版 www.workbuddy.ai）共用一套语义与形态，
// 故单列一个包，避免两边各写一份后漂移（R116）。
//
// 官方客户端（桌面 5.8.4 / CLI）一次对话请求带四个 id，分三层：
//
//	X-Conversation-ID          会话级：同一次会话恒定（官方 = 会话的 session.id）
//	X-Conversation-Request-ID  轮级：一次 user send 一个值，工具回调/重试复用
//	X-Conversation-Message-ID  调用级：每次模型请求新值
//	X-Request-ID               = X-Conversation-Message-ID
//	X-Root-Request-ID          = 本轮的 X-Conversation-Request-ID
//
// 形态取自官方抓包：会话 id 是带连字符的 UUID（v4/v7 均被上游接受，版本位不参与
// 校验），其余三个是 32 位无连字符 hex。参考实现
// ardeyouxipianyi/workbuddy2api-hub#381、CangShui/workbuddy-gateway、
// ithtelab/workbuddy-manager#193、@magpie-community/opencode-workbuddy-auth 一致。
//
// 为什么要「会话级恒定」：上游把 X-Conversation-Request-ID 当作「一次 user send =
// 一次请求」的计数键，把 X-Conversation-ID 当作会话归属。每请求现生成等于把一次
// 多轮对话拆成上千个一次性会话——那是反作弊视角下最明显的非人类形态（hub 原文）。
//
// 本包只做「值」：锚点抽取 Anchors、派生 ConversationID/TurnID、随机 MessageID。
package wbid

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// maxAnchorBody 超限不做锚点抽取：这种体积几乎都是内联图片，解析成本不值，
// 退回「每请求随机」的旧行为即可（缺锚点不影响上游校验，只影响像不像真人）。
const maxAnchorBody = 8 << 20

// Anchors 从 OpenAI 形状的 chat 请求体里抽两个锚点：
//
//	conv = 首条 user 消息文本的指纹 —— 同一会话的多轮对话里恒定
//	turn = 末条 user 消息文本的指纹 —— 一次 user send 内恒定（工具回调追加的是
//	       assistant/tool 消息，末条 user 仍在，故一轮多步复用同一个 turn）
//
// 取不到（无 messages / 解析失败 / 无文本 / body 超限）时返回空串，调用方退回随机。
// 锚点只在本地参与哈希，原文不上送、不落盘。
func Anchors(body []byte) (conv, turn string) {
	if len(body) == 0 || len(body) > maxAnchorBody {
		return "", ""
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return "", ""
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		text := contentText(m.Content)
		if text == "" {
			continue
		}
		if conv == "" {
			conv = fingerprint(text)
		}
		turn = fingerprint(text)
	}
	if conv == "" {
		// 无 user 消息（少见的单轮 system/assistant 形态）：退化为首条消息当锚点。
		if text := contentText(req.Messages[0].Content); text != "" {
			conv, turn = fingerprint(text), fingerprint(text)
		}
	}
	return conv, turn
}

// fingerprint 锚点文本 → 16 位 hex（sha256 前 8 字节）。截断只为短，碰撞概率可忽略。
func fingerprint(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// contentText 提取消息文本：content 可能是裸字符串，也可能是分片数组
// （多模态 / 工具结果）。图片等非文本分片不参与锚点。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	out := ""
	for _, p := range parts {
		if p.Text != "" {
			out += p.Text
		}
	}
	return out
}

// ConversationID 会话级 id：按「账号 + 会话锚点」稳定派生带连字符的 UUIDv4。
//
// 掺 uid 是刻意的：同一段对话被多个账号服务时不能共用同一个 id——那才是把会话
// 关联起来的键（hub 的注释同口径）。锚点为空时退回随机 v4（与旧行为等价）。
func ConversationID(uid, anchor string) string {
	if anchor == "" {
		return randomUUIDv4()
	}
	sum := sha256.Sum256([]byte("wb-conv\x00" + uid + "\x00" + anchor))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b)
}

// TurnID 轮级 id：32 位 hex，按「账号 + 会话 + 本轮锚点」稳定派生。
// 重试、换号、工具回调在同一次 user send 内复用同一个值（上游按它计数一次 send）。
func TurnID(uid, conv, anchor string) string {
	if anchor == "" {
		return MessageID()
	}
	sum := sha256.Sum256([]byte("wb-turn\x00" + uid + "\x00" + conv + "\x00" + anchor))
	return hex.EncodeToString(sum[:16])
}

// MessageID 调用级 id：32 位 hex，每次上游请求新生成（crypto/rand）。
func MessageID() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand 失败时退回时间戳派生（与历史实现同源思路），保持 32 hex 形态。
		n := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(n >> (i * 8))
			b[15-i] = byte(n >> ((7 - i) * 8))
		}
	}
	return hex.EncodeToString(b[:])
}

func randomUUIDv4() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(n >> (i * 8))
			b[15-i] = byte(n >> ((7 - i) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 36)
	j := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hexdigits[v>>4]
		out[j+1] = hexdigits[v&0x0f]
		j += 2
	}
	return string(out)
}
