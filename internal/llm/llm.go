// Package llm 定义模型提供方接口，并实现 OpenAI Chat Completions 兼容协议。
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

// Request 是一次与具体厂商无关的补全请求。
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
}

func (o *OpenAI) Complete(ctx context.Context, req Request) (string, error) {
	text, err := o.call(ctx, req, req.JSON)
	// 部分兼容网关不认 response_format，会返回 400，此时去掉该字段再试一次。
	var se *httpx.StatusError
	if err != nil && req.JSON && errors.As(err, &se) && se.Code == http.StatusBadRequest {
		return o.call(ctx, req, false)
	}
	return text, err
}

func (o *OpenAI) call(ctx context.Context, req Request, jsonMode bool) (string, error) {
	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"temperature": 0.2,
	}
	if jsonMode {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	data, err := httpx.Do(ctx, o.HTTP, func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+o.APIKey)
		r.Header.Set("Content-Type", "application/json")
		return r, nil
	})
	if err != nil {
		return "", err
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析模型响应失败: %w", err)
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", errors.New("模型返回内容为空")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}
