// Package llm 定义模型提供方接口，并实现 OpenAI Chat Completions 兼容协议。
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/httpx"
)

const (
	// maxSSELineBytes 是 SSE 单行（单个 data chunk）的最大字节数。
	// 慢思考模型可能把较大内容合并进单条 chunk，超过默认 64KB 会触发 bufio.ErrTooLong。
	maxSSELineBytes = 8 << 20
	// maxSSETotalBytes 是拼接后内容的总上限，避免无限制缓冲异常流。
	maxSSETotalBytes = 32 << 20
	// defaultStreamIdleTimeout 是 SSE 流式读取的空闲超时默认值。
	// 上游建立 SSE 连接后若中途不再吐字，客户端最多等待该时长后主动中断并重试，
	// 避免一直阻塞到整个 http.Client.Timeout（5 分钟）才失败。
	defaultStreamIdleTimeout = 45 * time.Second
)

// Request 是一次与厂商无关的补全请求。
type Request struct {
	Model  string
	System string
	User   string
	// JSON 为 true 时请求模型进入 JSON 输出模式。
	JSON bool
}

// Provider 是模型提供方。
type Provider interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// OpenAI 对接 OpenAI 及兼容该协议的网关、代理和本地模型服务。
type OpenAI struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	// StreamIdleTimeout 是 SSE 流式读取的空闲超时；零值或负值时使用 defaultStreamIdleTimeout。
	// 单元测试可注入较短超时以加速验证。
	StreamIdleTimeout time.Duration
}

// streamIdleTimeout 返回实际生效的 SSE 空闲超时。
func (o *OpenAI) streamIdleTimeout() time.Duration {
	if o.StreamIdleTimeout > 0 {
		return o.StreamIdleTimeout
	}
	return defaultStreamIdleTimeout
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (string, error) {
	// 默认启用 SSE 流式，避免慢思考模型在网关侧触发非流式硬超时。
	text, err := o.call(ctx, req, req.JSON, true)
	// 部分兼容网关不认 response_format，会返回 400，此时去掉该字段再试一次。
	if isBadRequest(err) && req.JSON {
		text, err = o.call(ctx, req, false, true)
	}
	// 仍有网关不支持 stream: true（返回 400），回退到非流式模式。
	if isBadRequest(err) {
		text, err = o.call(ctx, req, req.JSON, false)
	}
	if isBadRequest(err) && req.JSON {
		text, err = o.call(ctx, req, false, false)
	}
	return text, err
}

func isBadRequest(err error) bool {
	var se *httpx.StatusError
	return errors.As(err, &se) && se.Code == http.StatusBadRequest
}

func (o *OpenAI) call(ctx context.Context, req Request, jsonMode, stream bool) (string, error) {
	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"temperature": 0.2,
	}
	if stream {
		payload["stream"] = true
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	accept := "application/json"
	if stream {
		accept = "text/event-stream, application/json"
	}

	var text string
	err = httpx.DoStream(ctx, o.HTTP, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+o.APIKey)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", accept)
		return r, nil
	}, func(resp *http.Response) error {
		if stream && isSSE(resp) {
			var err error
			text, err = parseSSE(resp.Body, o.streamIdleTimeout())
			return err
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		text, err = parseNonStream(data)
		return err
	}, httpx.RetryUnsafe()) // 补全请求没有副作用，可以放心重发
	if err != nil {
		return "", err
	}
	return text, nil
}

func isSSE(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// parseNonStream 解析普通非流式 JSON 响应（choices[0].message.content）。
// 解析失败或内容为空时返回 *httpx.NonRetryable，避免重试同样的请求。
func parseNonStream(data []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", &httpx.NonRetryable{Err: fmt.Errorf("解析模型响应失败: %w", err)}
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", &httpx.NonRetryable{Err: errors.New("模型返回内容为空")}
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// idleTimeoutReader 包装流式响应体，为两次有效 data 事件之间的间隔增加空闲超时保护。
// 一旦超过 timeout 没有读到任何有效 data 事件，就置位 timedOut 并关闭底层 closer，
// 解除正在阻塞的 Read；": keepalive" 等心跳字节不会重置计时器，因此上游挂死但
// 网关持续发心跳时仍能按时中断并重试。
type idleTimeoutReader struct {
	r        io.Reader
	closer   io.Closer
	timer    *time.Timer
	timeout  time.Duration
	timedOut atomic.Bool
}

func newIdleTimeoutReader(r io.Reader, closer io.Closer, timeout time.Duration) *idleTimeoutReader {
	tr := &idleTimeoutReader{r: r, closer: closer, timeout: timeout}
	tr.timer = time.AfterFunc(timeout, func() {
		tr.timedOut.Store(true)
		if tr.closer != nil {
			_ = tr.closer.Close()
		}
	})
	return tr
}

// Read 只是透传底层读取；空闲计时器由 parseSSE 在读到有效 data 事件时显式调用 reset 重置。
func (tr *idleTimeoutReader) Read(p []byte) (int, error) {
	return tr.r.Read(p)
}

// reset 在读到有效 data 事件时重置空闲计时器；超时已触发则不再重复重置。
func (tr *idleTimeoutReader) reset() {
	if tr.timedOut.Load() {
		return
	}
	tr.timer.Reset(tr.timeout)
}

func (tr *idleTimeoutReader) stop() { tr.timer.Stop() }

// parseSSE 解析 OpenAI SSE（text/event-stream）响应，拼接 choices[0].delta.content。
// 忽略空行、":" 开头的注释/心跳行与 reasoning_content 等思考字段；遇到 [DONE] 正常结束；
// 若某 chunk 含顶层 error 对象则立即返回错误；拼接结果为空返回错误。
// idle 是流式空闲超时：只有扫描到非空 data: 行（含 reasoning_content、content 或 [DONE]）
// 才重置空闲计时器，空行与 ": keepalive" 心跳不重置；任何返回路径上只要空闲超时触发，
// 都必须优先返回可重试错误，绝不把截断后的部分内容当作成功返回。
func parseSSE(r io.Reader, idle time.Duration) (out string, err error) {
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	var closer io.Closer
	if c, ok := r.(io.Closer); ok {
		closer = c
	}
	tr := newIdleTimeoutReader(r, closer, idle)
	defer tr.stop()
	defer func() {
		// 空闲超时优先级最高：即使提前 EOF、返回了错误对象或已拼接部分内容，也绝不当作成功。
		if tr.timedOut.Load() {
			out, err = "", fmt.Errorf("模型流式响应空闲超时（超过 %s 无新数据）", idle)
		}
	}()

	scanner := bufio.NewScanner(tr)
	scanner.Buffer(make([]byte, 64*1024), maxSSELineBytes)
	var sb strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		tr.reset()
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// 非 JSON 的 data 行（异常心跳等）直接忽略，不打断正常流。
			continue
		}
		if chunk.Error != nil {
			msg := chunk.Error.Message
			if msg == "" {
				msg = chunk.Error.Type
			}
			return "", &httpx.NonRetryable{Err: fmt.Errorf("模型流式响应返回错误: %s", msg)}
		}
		if len(chunk.Choices) > 0 {
			if sb.Len()+len(chunk.Choices[0].Delta.Content) > maxSSETotalBytes {
				return "", &httpx.NonRetryable{Err: errors.New("模型流式响应内容超过上限")}
			}
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if err := scanner.Err(); err != nil {
		// 读取层面的错误按传输错误返回，交给 httpx 按 safe 决定是否重试。
		return "", fmt.Errorf("读取模型流式响应失败: %w", err)
	}
	out = strings.TrimSpace(sb.String())
	if out == "" {
		return "", &httpx.NonRetryable{Err: errors.New("模型返回内容为空")}
	}
	return out, nil
}
