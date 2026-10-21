package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProgressCallback 下载进度回调。
type ProgressCallback func(downloaded, total int64, speedBytesPerSec int64, source string)

// DownloadResult 下载完成结果。
type DownloadResult struct {
	FilePath string
	Source   string
}

// DownloadAsset 执行自动下载：直连优先 + 代理竞速兜底 + SHA256 校验。
func DownloadAsset(
	ctx context.Context,
	client *http.Client,
	asset *AssetInfo,
	proxies []string,
	onProgress ProgressCallback,
) (*DownloadResult, error) {
	if asset == nil || asset.DownloadURL == "" {
		return nil, fmt.Errorf("invalid asset: empty download url")
	}
	if client == nil {
		client = &http.Client{}
	}

	directURL := asset.DownloadURL
	var candidateURLs []struct {
		name string
		url  string
	}
	candidateURLs = append(candidateURLs, struct {
		name string
		url  string
	}{name: "direct", url: directURL})

	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.HasSuffix(p, "/") {
			p += "/"
		}
		candidateURLs = append(candidateURLs, struct {
			name string
			url  string
		}{
			name: p,
			url:  p + directURL,
		})
	}

	// ① 先试直连（2.5s 连接与首字节判定窗口）
	directCtx, directCancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	directResp, directErr := startDownloadStream(directCtx, client, directURL)
	if directErr == nil && directResp.StatusCode == http.StatusOK {
		// 直连可用，继续使用直连下载
		directCancel()
		res, err := saveAndVerify(ctx, directResp.Body, directResp.ContentLength, asset, "github-direct", onProgress)
		if err == nil {
			return res, nil
		}
		// 直连在传输中途失败或哈希不匹配，继续回退到代理竞速
	} else {
		directCancel()
		if directResp != nil && directResp.Body != nil {
			directResp.Body.Close()
		}
	}

	// ② 并行发起代理竞速（除去直连外的所有代理）
	proxyCandidates := candidateURLs[1:]
	if len(proxyCandidates) == 0 {
		return nil, fmt.Errorf("direct download failed and no proxies configured")
	}

	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()

	type raceWinner struct {
		name string
		resp *http.Response
	}
	winCh := make(chan raceWinner, 1)
	var winOnce sync.Once

	for _, c := range proxyCandidates {
		go func(target struct{ name, url string }) {
			req, err := http.NewRequestWithContext(raceCtx, http.MethodGet, target.url, nil)
			if err != nil {
				return
			}
			req.Header.Set("User-Agent", "wild-work-updater")

			resp, err := client.Do(req)
			if err != nil {
				return
			}
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				return
			}

			winOnce.Do(func() {
				winCh <- raceWinner{name: target.name, resp: resp}
			})
		}(c)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case winner := <-winCh:
		// 胜出者：取消其他竞速请求，保存并校验数据流
		defer winner.resp.Body.Close()
		raceCancel()
		return saveAndVerify(ctx, winner.resp.Body, winner.resp.ContentLength, asset, winner.name, onProgress)
	case <-time.After(15 * time.Second):
		return nil, fmt.Errorf("timeout waiting for download proxy response")
	}
}

func startDownloadStream(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "wild-work-updater")
	return client.Do(req)
}

func saveAndVerify(
	ctx context.Context,
	body io.Reader,
	contentLength int64,
	asset *AssetInfo,
	sourceName string,
	onProgress ProgressCallback,
) (*DownloadResult, error) {
	tmpFile, err := os.CreateTemp("", "wild-work-update-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
	}()

	total := contentLength
	if total <= 0 && asset.Size > 0 {
		total = asset.Size
	}

	var downloaded atomic.Int64
	startTime := time.Now()
	var lastTickTime time.Time
	var lastTickBytes int64

	buf := make([]byte, 64*1024)
	doneCh := make(chan struct{})
	defer close(doneCh)

	// 进度上报循环
	if onProgress != nil {
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-doneCh:
					return
				case <-ticker.C:
					now := time.Now()
					curr := downloaded.Load()
					var speed int64
					if !lastTickTime.IsZero() {
						elapsedSec := now.Sub(lastTickTime).Seconds()
						if elapsedSec > 0 {
							speed = int64(float64(curr-lastTickBytes) / elapsedSec)
						}
					} else {
						elapsedSec := now.Sub(startTime).Seconds()
						if elapsedSec > 0 {
							speed = int64(float64(curr) / elapsedSec)
						}
					}
					lastTickTime = now
					lastTickBytes = curr
					onProgress(curr, total, speed, sourceName)
				}
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			_ = os.Remove(tmpPath)
			return nil, ctx.Err()
		default:
		}

		n, rErr := body.Read(buf)
		if n > 0 {
			if _, wErr := tmpFile.Write(buf[:n]); wErr != nil {
				_ = os.Remove(tmpPath)
				return nil, fmt.Errorf("write temp file: %w", wErr)
			}
			downloaded.Add(int64(n))
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("read stream: %w", rErr)
		}
	}

	_ = tmpFile.Close()

	if onProgress != nil {
		onProgress(downloaded.Load(), total, 0, sourceName)
	}

	// SHA256 校验
	if asset.Digest != "" {
		if err := VerifyDigest(tmpPath, asset.Digest); err != nil {
			_ = os.Remove(tmpPath)
			return nil, fmt.Errorf("digest verification failed: %w", err)
		}
	}

	return &DownloadResult{
		FilePath: tmpPath,
		Source:   sourceName,
	}, nil
}
