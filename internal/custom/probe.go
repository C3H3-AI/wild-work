package custom

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// 上游 /models 里各家网关的能力字段别名表。
//
// 为什么要别名表而不是「就认 OpenAI 官方形状」：官方 /models 根本不返回能力，
// 而第三方网关大量私扩字段——用户想要的「能拿到能力就一并拉」只能靠宽容匹配实现。
// 取值为点分路径（支持嵌套对象），命中第一个非空即用；顺序 = 优先级
// （越靠前的越权威，避免 `properties` 里的猜测覆盖顶层的官方声明）。
var (
	ctxKeys    = []string{"context_window", "context_length", "max_input_tokens", "architecture.input_context", "top_provider.context_length", "properties.context_window"}
	outKeys    = []string{"max_tokens", "max_completion_tokens", "output_context", "architecture.output_context", "top_provider.max_completion_tokens", "properties.max_tokens"}
	imagesKeys = []string{"supports_images", "vision", "capabilities.image_input", "supports_vision"}
	toolsKeys  = []string{"supports_tools", "function_calling", "capabilities.tools"}
	rsnKeys    = []string{"supports_reasoning", "capabilities.reasoning"}
	effortKeys = []string{"reasoning_efforts", "supported_reasoning_levels", "supported_reasoning_efforts", "capabilities.reasoning_efforts"}
	// modality/参数列表类：值本身是数组或 `text+image->text` 这种拼接串，靠子串匹配。
	listKeys = []string{"supported_parameters", "supported_features", "capabilities", "architecture.input_modalities", "features", "tags"}
	defEff   = []string{"default_reasoning_effort", "reasoning.default_effort", "supported_reasoning_level"}
	nameKeys = []string{"name", "display_name", "title", "alias"}
	idKeys   = []string{"id", "model", "model_id", "slug"}
)

// parseModelsPayload 宽容解析上游 /models 响应体，返回尽力提取的能力清单。
//
// 只解析、不发请求：抽成纯函数是为了能拿各家真实响应样本做单测（不必起 http）。
// 解析不到任何能力字段的条目仍会返回（ID 非空即可），CapSource 留空 = 「未探测」。
func parseModelsPayload(raw []byte) ([]ModelCap, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("上游返回的不是合法 JSON：%w", err)
	}
	list := modelList(v)
	out := make([]ModelCap, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue // 数组里混进字符串等异常形状，跳过而不是让整次探测失败
		}
		c := ModelCap{}
		c.ID = strings.TrimSpace(firstString(m, idKeys))
		if c.ID == "" {
			continue
		}
		c.DisplayName = strings.TrimSpace(firstString(m, nameKeys))
		if c.DisplayName == c.ID {
			c.DisplayName = "" // 展示名与 ID 相同不重复下发
		}
		c.ContextWindow = firstTokens(m, ctxKeys)
		c.MaxTokens = firstTokens(m, outKeys)
		c.SupportsImages = anyCapability(m, imagesKeys, listKeys, "image", "vision")
		c.SupportsReasoning = anyCapability(m, rsnKeys, listKeys, "reasoning", "thinking", "think")
		if tools, ok := anyCapabilityOK(m, toolsKeys, listKeys, "tools", "tool_calls", "function_call", "function_calling"); ok {
			t := tools
			c.SupportsTools = &t
		}
		c.ReasoningEfforts = provider.CleanEfforts(effortList(m, effortKeys))
		c.DefaultReasoningEffort = strings.ToLower(strings.TrimSpace(firstString(m, defEff)))
		if len(c.ReasoningEfforts) == 0 && c.SupportsReasoning {
			// 声明了支持思考但没给档位表 —— 支持位仍为真，档位保持「上游未声明」（R49：不编档位列表）
			c.ReasoningEfforts = nil
		}
		// 默认档位不在上游声明的档位表里 = 上游自相矛盾，宁可不写也不能拿它去覆盖一个有效档位
		if c.DefaultReasoningEffort != "" && !containsFold(c.ReasoningEfforts, c.DefaultReasoningEffort) {
			c.DefaultReasoningEffort = ""
		}
		c.finalize()
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("上游未返回可用的模型列表（响应里没有带 id 的条目）")
	}
	return out, nil
}

// modelList 从响应体里取出模型数组：`{"data":[...]}` / `{"models":[...]}` / `[...]` 三种外层形态都吃。
func modelList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, k := range []string{"data", "models", "items", "result"} {
			switch sub := t[k].(type) {
			case []any:
				return sub
			case map[string]any: // 再套一层（如 {"result":{"data":[...]}}）
				if l := modelList(sub); len(l) > 0 {
					return l
				}
			}
		}
	}
	return nil
}

// dig 按点分路径取嵌套字段（"architecture.input_context"）。
func dig(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func firstString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := dig(m, k); ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// firstTokens 按别名顺序取第一个能解析成 token 数的字段（数字或 "1M"/"64k" 带单位串）。
func firstTokens(m map[string]any, keys []string) int64 {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok || v == nil {
			continue
		}
		if n, err := ParseTokens(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// capabilityListText 把 listKeys 里那些数组/字符串字段拼成一段小写文本，供子串匹配。
//
// 注意 `capabilities` 既可能是对象（{"image_input":true}）也可能是字符串数组，
// 这里只取字符串元素与 true 的布尔键名——布尔 false 不参与匹配，
// 否则 "supports_images":false 会被子串命中而误判成「支持识图」。
func capabilityBlob(m map[string]any, listKeys []string) string {
	var sb strings.Builder
	for _, k := range listKeys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			sb.WriteString(strings.ToLower(t))
			sb.WriteByte(' ')
		case []any:
			for _, e := range t {
				if s, ok := e.(string); ok {
					sb.WriteString(strings.ToLower(s))
					sb.WriteByte(' ')
				}
			}
		case map[string]any:
			for name, val := range t {
				if b, ok := val.(bool); ok && b {
					sb.WriteString(strings.ToLower(name))
					sb.WriteByte(' ')
				}
			}
		}
	}
	return sb.String()
}

// boolAt 取显式布尔声明（只有值为 true 才算「声明支持」；false/非布尔 = 未声明）。
func boolAt(m map[string]any, keys []string) (bool, bool) {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		switch b := v.(type) {
		case bool:
			return b, true
		case string:
			if parsed, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
				return parsed, true
			}
		}
	}
	return false, false
}

// anyCapability 「是否为真」：显式布尔为真，或列表字段里出现任一标记子串。
func anyCapability(m map[string]any, boolKeys, listKeys []string, markers ...string) bool {
	v, ok, _ := anyCapabilityHit(m, boolKeys, listKeys, markers...)
	return ok && v
}

// anyCapabilityOK 返回 (值, 是否声明)。上游显式写了 false 也算「声明」——
// 与「上游根本没提」不同：前者可以如实写进配置（不支持工具），后者必须留空。
func anyCapabilityOK(m map[string]any, boolKeys, listKeys []string, markers ...string) (bool, bool) {
	v, ok, hit := anyCapabilityHit(m, boolKeys, listKeys, markers...)
	if hit {
		return true, true
	}
	return v, ok
}

func anyCapabilityHit(m map[string]any, boolKeys, listKeys []string, markers ...string) (val bool, declared bool, listHit bool) {
	if b, ok := boolAt(m, boolKeys); ok {
		return b, true, false
	}
	blob := capabilityBlob(m, listKeys)
	for _, mk := range markers {
		if strings.Contains(blob, strings.ToLower(mk)) {
			return true, true, true
		}
	}
	return false, false, false
}

// effortList 取档位表：既接受 ["low","high"]，也接受 [{"name":"Low"},{"level":"high"}]。
func effortList(m map[string]any, keys []string) []string {
	for _, k := range keys {
		v, ok := dig(m, k)
		if !ok {
			continue
		}
		arr, ok := v.([]any)
		if !ok {
			continue
		}
		var out []string
		for _, e := range arr {
			switch t := e.(type) {
			case string:
				// 与下面对象分支同口径统一小写：全仓档位取值为 lowercase（gemini TierLow /
				// R63 出站注入都是小写），上游写 "Low" 也得归一，否则前端勾上后原样发出会被拒。
				out = append(out, strings.ToLower(strings.TrimSpace(t)))
			case map[string]any:
				// 档位对象：优先 type/level/name/value/id 里第一个字符串
				for _, kk := range []string{"type", "level", "value", "name", "id"} {
					if s, ok := t[kk].(string); ok && s != "" {
						// "Low"/"MEDIUM" 统一小写，与 /v1/models 的 reasoning_effort 取值口径一致
						out = append(out, strings.ToLower(s))
						break
					}
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// TestConnection 拉一次上游 /models：返回尽力解析出的能力清单，兼做连通性测试。
//
// 这是**用户自己的第三方端点**，不参与内建渠道的风控账号池，因此不受 §0.1
// 「严禁穷举探测上游接口」约束；但仍然只发一个请求、不穷举多个候选端点
// （OpenRouter 式扩展字段本就在同一条 /models 响应里，没有第二个请求的必要）。
func (c *Client) TestConnection() ([]ModelCap, error) {
	endpoint := strings.TrimRight(c.cfg.BaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	// 探测是面板上的一次用户点击动作，15s 足够；不等 StreamHTTP（那是对话流用的）。
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接失败：%w", err)
	}
	defer resp.Body.Close()
	// 上游可能是网关错误页/HTML，限 1 MiB 再判：整段读回只为拼进错误消息（不解析）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return parseModelsPayload(raw)
}

// containsFold 不区分大小写的成员判定（档位已统一小写，这里只防上游脏数据）。
func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
