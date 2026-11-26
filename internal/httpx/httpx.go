// Package httpx 封装带重试的 HTTP 请求。
package httpx

import (
	"context"
	"errors"
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

// Do 发送请求并返回响应体，失败时按指数退避重试，最多 maxAttempts 次。
// 429 对所有方法都会重试；网络错误、408 和 5xx 只对幂等方法（或使用了
// RetryUnsafe 的请求）重试，避免 POST 重发造成重复评论。其余错误状态码
// 直接以 *StatusError 返回。newReq 每次尝试都会被重新调用，因此请求体可以重复读取。
func Do(ctx context.Context, c *http.Client, newReq func() (*http.Request, error), opts ...Option) ([]byte, error) {
	var body []byte
	err := doStream(ctx, c, newReq, func(resp *http.Response) error {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return readErr
		}
		body = data
		return nil
	}, opts...)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// NonRetryable 包装一个内容级错误（区别于传输层错误），标记该错误不应触发重试。
// 用于 DoStream 的流式解析回调：例如模型返回了空内容或流中途返回错误对象时，
// 重试同样的请求没有意义，应当直接上抛。
type NonRetryable struct{ Err error }

func (e *NonRetryable) Error() string { return e.Err.Error() }
func (e *NonRetryable) Unwrap() error { return e.Err }

// DoStream 与 Do 类似，但收到 2xx 响应时不读入整个响应体，而是把响应交给
// handle 处理（handle 负责读取响应体，DoStream 会在返回前关闭）。适用于需要
// 边接收边处理的 SSE 流式响应。handle 返回普通 error 视为传输层错误，按 safe
// 决定是否重试；返回 *NonRetryable 包装的错误则不重试直接返回。
func DoStream(ctx context.Context, c *http.Client, newReq func() (*http.Request, error), handle func(*http.Response) error, opts ...Option) error {
	return doStream(ctx, c, newReq, handle, opts...)
}

// doStream 是 Do 与 DoStream 共享的请求循环：发送请求、处理重试与状态码，
// 收到 2xx 响应后调用 handle 处理。
func doStream(ctx context.Context, c *http.Client, newReq func() (*http.Request, error), handle func(*http.Response) error, opts ...Option) error {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
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
				return ctx.Err()
			}
		}
		wait = 0

		req, err := newReq()
		if err != nil {
			return err
		}
		safe := o.retryUnsafe || idempotent(req.Method)
		resp, err := c.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 请求可能已经送达服务端，非幂等方法不能盲目重发
			if !safe {
				return err
			}
			lastErr = err
			continue
		}
		if resp.StatusCode < 300 {
			err := handle(resp)
			resp.Body.Close()
			if err == nil {
				return nil
			}
			var nr *NonRetryable
			if errors.As(err, &nr) || !safe {
				return err
			}
			lastErr = err
			continue
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			if !safe {
				return readErr
			}
			lastErr = readErr
			continue
		}
		se := &StatusError{
			Method: req.Method,
			Path:   req.URL.Path,
			Code:   resp.StatusCode,
			Body:   textutil.TruncateUTF8(string(data), 500),
		}
		if !retryable(resp.StatusCode, safe) {
			return se
		}
		lastErr = se
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
	}
	return lastErr
}

// retryable 判断状态码是否值得重试。429 表示请求被限流、没有被处理，任何方法都可以重试；
// 408 和 5xx 无法确定服务端是否已经处理，只对安全的请求重试。
func retryable(code int, safe bool) bool {
	if code == http.StatusTooManyRequests {
		return true
	}
	return safe && (code == http.StatusRequestTimeout || code >= 500)
}

// idempotent 报告该方法重发是否安全。本项目里的 PATCH 都是整体替换评论正文，视为幂等。
func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

type options struct{ retryUnsafe bool }

// Option 调整 Do 的行为。
type Option func(*options)

// RetryUnsafe 让非幂等请求（如 POST）在网络错误和 5xx 时也重试。
// 仅用于重发没有副作用的请求，比如模型补全。
func RetryUnsafe() Option { return func(o *options) { o.retryUnsafe = true } }
