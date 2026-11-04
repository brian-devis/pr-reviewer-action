// Package httpx 封装带重试的 HTTP 请求。
package httpx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/textutil"
)

const (
	maxAttempts  = 4
	maxRetryWait = 30 * time.Second
)

// baseDelay 是首次重试前的等待时间，之后每次翻倍；测试中会调小。
var baseDelay = time.Second

// StatusError 表示服务端返回了非 2xx 状态码。
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s 返回 %d: %s", e.Method, e.Path, e.Code, e.Body)
}

// Do 发送请求并返回响应体。网络错误、408、429 和 5xx 会按指数退避重试，
// 其余错误状态码直接以 *StatusError 返回。newReq 每次尝试都会被重新调用，
// 因此请求体可以重复读取。
func Do(ctx context.Context, c *http.Client, newReq func() (*http.Request, error)) ([]byte, error) {
	var lastErr error
	var wait time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			d := wait
			if d <= 0 {
				d = baseDelay << (attempt - 1)
			}
			if d > maxRetryWait {
				d = maxRetryWait
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		wait = 0

		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode < 300 {
			return data, nil
		}
		se := &StatusError{
			Method: req.Method,
			Path:   req.URL.Path,
			Code:   resp.StatusCode,
			Body:   textutil.TruncateUTF8(string(data), 500),
		}
		if !retryable(resp.StatusCode) {
			return nil, se
		}
		lastErr = se
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	return nil, lastErr
}

func retryable(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}
