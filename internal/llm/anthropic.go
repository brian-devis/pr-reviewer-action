package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/brian-devis/pr-reviewer-action/internal/httpx"
)

const (
	anthropicVersion = "2023-06-01"
	// anthropicMaxTokens 是单次回复的输出上限；Messages API 要求必须提供。
	// 一批最多 15 条评论的 JSON 远小于这个值，留足余量避免被截断。
	anthropicMaxTokens = 8192
)

// Anthropic 对接 Anthropic 原生 Messages API。
type Anthropic struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func (a *Anthropic) Complete(ctx context.Context, req Request) (string, error) {
	// Messages API 没有 JSON 模式，req.JSON 由 prompt 约束，解析端容忍多余文字。
	payload := map[string]any{
		"model":       req.Model,
		"max_tokens":  anthropicMaxTokens,
		"temperature": 0.2,
		"messages":    []map[string]string{{"role": "user", "content": req.User}},
	}
	if req.System != "" { // 空的 system 会被 API 拒绝
		payload["system"] = req.System
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	data, err := httpx.Do(ctx, a.HTTP, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("x-api-key", a.APIKey)
		r.Header.Set("anthropic-version", anthropicVersion)
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	}, httpx.RetryUnsafe()) // 补全请求没有副作用，可以放心重发
	if err != nil {
		return "", err
	}

	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析模型响应失败: %w", err)
	}
	if out.StopReason == "max_tokens" {
		// 输出被截断，JSON 必然不完整，直接给出明确原因
		return "", fmt.Errorf("模型输出被 max_tokens（%d）截断，请减小 max_diff_bytes", anthropicMaxTokens)
	}
	// 回复由多个内容块组成，只取文本块（跳过 thinking 等其他类型）
	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("模型返回内容为空")
	}
	return strings.TrimSpace(text.String()), nil
}

// endpoint 兼容两种写法：https://api.anthropic.com 和 https://gateway.example.com/v1。
func (a *Anthropic) endpoint() string {
	if strings.HasSuffix(a.BaseURL, "/v1") {
		return a.BaseURL + "/messages"
	}
	return a.BaseURL + "/v1/messages"
}
