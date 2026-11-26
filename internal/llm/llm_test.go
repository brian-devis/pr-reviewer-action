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

func TestCompleteSendsAuthAndParsesContent(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{"choices":[{"message":{"content":"  hello  "}}]}`))
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "sk-test", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m1", System: "s", User: "u", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("content = %q", got)
	}
	if gotAuth != "Bearer sk-test" || gotPath != "/chat/completions" {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}
	if !strings.Contains(gotBody, `"model":"m1"`) || !strings.Contains(gotBody, "response_format") {
		t.Fatalf("请求体异常: %s", gotBody)
	}
}

func TestCompleteFallsBackWithoutResponseFormat(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "response_format") {
			http.Error(w, "unsupported parameter", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u", JSON: true})
	if err != nil || got != "ok" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestCompleteEmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(body))
	}))
}

func TestCompleteStreamingSSE(t *testing.T) {
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"stream":true`) {
			t.Errorf("请求未启用 stream: %s", b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, ": keepalive\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello world" {
		t.Fatalf("content = %q", got)
	}
	if !strings.Contains(gotAccept, "text/event-stream") {
		t.Fatalf("Accept 头应包含 text/event-stream，实际: %q", gotAccept)
	}
}

func TestCompleteStreamingSSECaseInsensitiveContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil || got != "ok" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestCompleteSSEIgnoresReasoningContent(t *testing.T) {
	srv := sseServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"A\",\"reasoning_content\":\"think\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"more\",\"content\":\"B\"}}]}\n\n"+
		"data: [DONE]\n\n")
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "AB" {
		t.Fatalf("content = %q, want %q", got, "AB")
	}
}

func TestCompleteSSEReturnsErrorOnErrorChunk(t *testing.T) {
	srv := sseServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+
		"data: {\"error\":{\"message\":\"上游不可用\",\"type\":\"api_error\"}}\n\n")
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"}); err == nil {
		t.Fatal("expected error for error chunk")
	}
}

func TestCompleteSSEEmptyContent(t *testing.T) {
	srv := sseServer(t, ": keepalive\n\n: keepalive\n\ndata: [DONE]\n\n")
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	if _, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"}); err == nil {
		t.Fatal("expected error for empty stream content")
	}
}

func TestCompleteSSELargeChunk(t *testing.T) {
	big := strings.Repeat("x", 70*1024)
	chunk, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{"content": big}}},
	})
	body := "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"
	srv := sseServer(t, body)
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got != big {
		t.Fatalf("large chunk 解析失败，长度 %d，want %d", len(got), len(big))
	}
}

func TestCompleteNonStreamJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"plain"}}]}`))
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "plain" {
		t.Fatalf("content = %q", got)
	}
}

func TestCompleteFallsBackToNonStreamOnStream400(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"stream":true`) {
			http.Error(w, "unsupported parameter", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client()}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil || got != "ok" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}
