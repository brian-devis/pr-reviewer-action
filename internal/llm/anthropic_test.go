package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnthropicRequestAndResponse(t *testing.T) {
	var path, key, version string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, version = r.URL.Path, r.Header.Get("x-api-key"), r.Header.Get("anthropic-version")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		io.WriteString(w, `{"content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"  {\"summary\":"},{"type":"text","text":"\"ok\"}  "}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	a := &Anthropic{BaseURL: srv.URL, APIKey: "sk-ant-test", HTTP: srv.Client()}
	got, err := a.Complete(context.Background(), Request{Model: "claude-x", System: "sys", User: "usr", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"summary":"ok"}` {
		t.Fatalf("应只拼接 text 块并去掉首尾空白，got %q", got)
	}
	if path != "/v1/messages" || key != "sk-ant-test" || version != anthropicVersion {
		t.Fatalf("path=%q key=%q version=%q", path, key, version)
	}
	if body["model"] != "claude-x" || body["system"] != "sys" || body["max_tokens"] != float64(anthropicMaxTokens) {
		t.Fatalf("请求体异常: %v", body)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "usr" {
		t.Fatalf("messages 异常: %v", body["messages"])
	}
	if _, has := body["response_format"]; has {
		t.Fatal("Messages API 不支持 response_format")
	}
}

func TestAnthropicEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://api.anthropic.com":      "https://api.anthropic.com/v1/messages",
		"https://gateway.example.com/v1": "https://gateway.example.com/v1/messages",
	}
	for base, want := range tests {
		if got := (&Anthropic{BaseURL: base}).endpoint(); got != want {
			t.Errorf("endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestAnthropicOmitsEmptySystem(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	a := &Anthropic{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := a.Complete(context.Background(), Request{Model: "m", User: "u"}); err != nil {
		t.Fatal(err)
	}
	if _, has := body["system"]; has {
		t.Fatal("system 为空时不应出现在请求体里")
	}
}

func TestAnthropicTruncatedByMaxTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"summary\":\"cut"}],"stop_reason":"max_tokens"}`)
	}))
	defer srv.Close()

	a := &Anthropic{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	_, err := a.Complete(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "max_tokens") {
		t.Fatalf("应返回明确的截断错误，got %v", err)
	}
}

func TestAnthropicEmptyContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[{"type":"thinking","thinking":"only thoughts"}]}`)
	}))
	defer srv.Close()

	a := &Anthropic{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := a.Complete(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("没有文本块时应报错")
	}
}

func TestAnthropicRetriesServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			http.Error(w, `{"type":"error","error":{"type":"overloaded_error"}}`, 529)
			return
		}
		io.WriteString(w, `{"content":[{"type":"text","text":"ok"}]}`)
	}))
	defer srv.Close()

	a := &Anthropic{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := a.Complete(context.Background(), Request{Model: "m"})
	if err != nil || got != "ok" || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("got=%q err=%v calls=%d", got, err, calls)
	}
}
