package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestMatchAsset(t *testing.T) {
	assets := []AssetInfo{
		{Name: "wild-work-darwin-arm64", Size: 100, DownloadURL: "http://example.com/darwin-arm64"},
		{Name: "wild-work-windows-amd64.exe", Size: 200, DownloadURL: "http://example.com/win-amd64"},
		{Name: "wild-work-linux-amd64", Size: 300, DownloadURL: "http://example.com/linux-amd64"},
		{Name: "WildWork-2.9.4-arm64.dmg", Size: 400, DownloadURL: "http://example.com/dmg"},
	}

	matchWin, err := MatchAsset(assets, "windows", "amd64")
	if err != nil || matchWin.Name != "wild-work-windows-amd64.exe" {
		t.Fatalf("expected windows-amd64, got %v, err %v", matchWin, err)
	}

	matchMac, err := MatchAsset(assets, "darwin", "arm64")
	if err != nil || matchMac.Name != "wild-work-darwin-arm64" {
		t.Fatalf("expected darwin-arm64, got %v, err %v", matchMac, err)
	}

	matchLinux, err := MatchAsset(assets, "linux", "amd64")
	if err != nil || matchLinux.Name != "wild-work-linux-amd64" {
		t.Fatalf("expected linux-amd64, got %v, err %v", matchLinux, err)
	}

	_, err = MatchAsset(assets, "freebsd", "riscv64")
	if err == nil {
		t.Fatalf("expected error for unsupported platform, got nil")
	}
}

func TestVerifyDigest(t *testing.T) {
	content := []byte("hello wild-work auto update")
	tmpFile, err := os.CreateTemp("", "test-digest-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	_, _ = tmpFile.Write(content)
	_ = tmpFile.Close()

	hasher := sha256.New()
	hasher.Write(content)
	expectedHex := hex.EncodeToString(hasher.Sum(nil))

	// sha256:xxx 格式
	if err := VerifyDigest(tmpFile.Name(), "sha256:"+expectedHex); err != nil {
		t.Fatalf("VerifyDigest with prefix failed: %v", err)
	}

	// 裸 hex 格式
	if err := VerifyDigest(tmpFile.Name(), expectedHex); err != nil {
		t.Fatalf("VerifyDigest bare hex failed: %v", err)
	}

	// 错误摘要
	if err := VerifyDigest(tmpFile.Name(), "sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatalf("expected digest error, got nil")
	}
}

func TestCheckSources(t *testing.T) {
	// 搭建 mock GitHub API 服务
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tag_name": "v2.9.5",
			"name": "v2.9.5 发布",
			"body": "更新内容测试",
			"published_at": "2026-10-21T10:00:00Z",
			"assets": [
				{
					"name": "wild-work-darwin-arm64",
					"size": 12345,
					"digest": "sha256:11223344",
					"browser_download_url": "http://example.com/download/wild-work-darwin-arm64"
				}
			]
		}`)
	})

	server := httptest.NewServer(apiHandler)
	defer server.Close()

	// 拦截或重定向测试 client
	client := &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			r := req.Clone(req.Context())
			r.URL.Scheme = "http"
			r.URL.Host = server.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(r)
		}),
	}

	rel, hasUpdate, err := CheckSources(context.Background(), client, "test/repo", "2.9.4", nil)
	if err != nil {
		t.Fatalf("CheckSources failed: %v", err)
	}
	if !hasUpdate {
		t.Fatalf("expected hasUpdate = true, got false")
	}
	if rel.Version != "2.9.5" {
		t.Fatalf("expected version 2.9.5, got %s", rel.Version)
	}
}

func TestDownloadAsset(t *testing.T) {
	fileContent := []byte("binary payload content")
	hasher := sha256.New()
	hasher.Write(fileContent)
	expectedHex := hex.EncodeToString(hasher.Sum(nil))

	dlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fileContent)
	}))
	defer dlServer.Close()

	asset := &AssetInfo{
		Name:        "wild-work-test",
		Size:        int64(len(fileContent)),
		Digest:      "sha256:" + expectedHex,
		DownloadURL: dlServer.URL + "/test-binary",
	}

	var progressCalls int
	res, err := DownloadAsset(context.Background(), http.DefaultClient, asset, nil, func(d, t, s int64, src string) {
		progressCalls++
	})
	if err != nil {
		t.Fatalf("DownloadAsset failed: %v", err)
	}
	defer os.Remove(res.FilePath)

	saved, err := os.ReadFile(res.FilePath)
	if err != nil || string(saved) != string(fileContent) {
		t.Fatalf("downloaded content mismatch, err: %v", err)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
