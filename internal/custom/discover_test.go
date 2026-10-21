package custom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Discover 的端点定位（R121）。全部走 httptest，不打上游。

// muxModels 只挂 /v1/models 的网关（OpenAI 官方与多数聚合网关的形状）。
func TestDiscoverPrefersRootWithModels(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-x","context_window":131072}]}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusMethodNotAllowed) // GET 打在 POST-only 路由上
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Discover: %v（tries=%+v）", err, d.Tries)
	}
	if d.Base != srv.URL+"/v1" || !d.Adjusted || !d.ModelsOK || !d.ChatOK {
		t.Fatalf("应自动补 /v1 并确认对话端点：%+v", d)
	}
	if len(d.Caps) != 1 || d.Caps[0].ContextWindow != 131072 || d.Caps[0].CapSource != CapSourceAPI {
		t.Fatalf("能力清单没带回来：%+v", d.Caps)
	}
	for _, h := range hits {
		if strings.Contains(h, "/chat/completions") && strings.Contains(h, "/v1/v1") {
			t.Fatalf("拼出了双根：%s", h)
		}
	}
	if len(d.Tries) < 3 {
		t.Fatalf("探测明细应留痕：%+v", d.Tries)
	}
}

// 用户粘了带尾巴的完整地址：静态规范化就该治好，不必换根。
func TestDiscoverStripsChatTail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL + "/v1/chat/completions"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Base != srv.URL+"/v1" || !d.Renormed || d.Adjusted || !d.ModelsOK {
		t.Fatalf("应只去尾巴不换根：%+v", d)
	}
}

// 网关不提供 /models（或 key 无目录权限）：退到「对话端点存在」确认根，并如实标注依据弱。
func TestDiscoverFallsBackToChatProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			w.WriteHeader(http.StatusNotFound) // /models 全线 404
		}
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if d.Base != srv.URL || !d.ChatOK || d.ModelsOK || d.Note == "" {
		t.Fatalf("应按对话端点确认且不谎称拿到目录：%+v", d)
	}
	if !strings.Contains(d.Note, "未开放模型目录") {
		t.Fatalf("Note 要说明确认依据弱：%q", d.Note)
	}
}

// SPA 兜底页陷阱：未知路径回 200 + HTML。没有带 id 的条目就不算命中，
// 否则会把这个根当成可用端点，对话时才发现全崩。
func TestDiscoverRejectsHTMLCaptivePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>控制台</body></html>`))
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL})
	if err == nil {
		t.Fatalf("200+HTML 不得算命中：%+v", d)
	}
	if !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "端点") {
		t.Fatalf("错误应指向没找到端点：%v", err)
	}
}

// 401/403 不能当成「端点存在」：网关在路由前鉴权时，401 对对错地址都长得一样，
// 拿它确认会把填错的 key 说成配好了。
func TestDiscoverAuthBlockedIsNotConfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL, APIKey: "sk-bad"})
	if err == nil {
		t.Fatalf("全线 401 必须报错：%+v", d)
	}
	if !strings.Contains(err.Error(), "api-key") {
		t.Fatalf("结论要指向密钥问题，不能含糊成连接失败：%v", err)
	}
	if d.ModelsOK || d.ChatOK {
		t.Fatalf("鉴权被拒不等于端点已确认：%+v", d)
	}
}

// 嗅探阶段绝不 POST 对话端点（会产生真实调用与费用）。
func TestDiscoverNeverPosts(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	if _, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL}); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("嗅探只准发 GET，实际发了 %s", m)
		}
	}
	if len(methods) > 6 {
		t.Fatalf("请求数应封在 6 次以内，实际 %d 次：%v", len(methods), methods)
	}
}

// 非法地址必须 Fail Early，不带着坏配置去发请求。
func TestDiscoverRejectsGarbage(t *testing.T) {
	if _, err := Discover(ChannelConfig{Name: "t", BaseURL: ""}); err == nil {
		t.Fatal("空接入点应报错")
	}
	if _, err := Discover(ChannelConfig{Name: "t", BaseURL: "not a url at all"}); err == nil {
		t.Fatal("含空白的非法地址应报错")
	}
}

// Bearer 头与请求路径形态（规范化后拼 /models 不出现双斜杠）。
func TestDiscoverSendsBearerAndCleanPath(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d, err := Discover(ChannelConfig{Name: "t", BaseURL: srv.URL + "/", APIKey: "sk-abcdefg1234567"})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if gotAuth != "Bearer sk-abcdefg1234567" || gotPath != "/models" {
		t.Fatalf("鉴权头或路径不对：%q %q", gotAuth, gotPath)
	}
	if d.Base != srv.URL {
		t.Fatalf("尾斜杠应被削掉：%+v", d)
	}
}
