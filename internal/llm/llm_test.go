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
	"time"
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

func TestCompleteIdleTimeoutRetriesAndSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if f != nil {
			f.Flush()
		}
		if n == 1 {
			// 首次请求发送部分 chunk 后挂起，模拟命中上游挂死账号。
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" done\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client(), StreamIdleTimeout: 120 * time.Millisecond}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "partial done" {
		t.Fatalf("content = %q", got)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestCompleteSSEIdleTimeoutNotTriggeredBySlowStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		chunks := []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n",
			": keepalive\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"B\"}}]}\n\n",
			": keepalive\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\"C\"}}]}\n\n",
			"data: [DONE]\n\n",
		}
		for _, c := range chunks {
			io.WriteString(w, c)
			if f != nil {
				f.Flush()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// data 行每约 100ms 到达一次（中间夹一条 keepalive），空闲超时须明显大于该间隔，
	// 以容忍不同平台 timer 分辨率带来的抖动，避免慢但活跃的流被误判为空闲超时。
	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client(), StreamIdleTimeout: 300 * time.Millisecond}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ABC" {
		t.Fatalf("content = %q", got)
	}
}

func TestParseSSEIdleTimeoutNoTruncatedContent(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		io.WriteString(pw, "data: {\"choices\":[{\"delta\":{\"content\":\"truncated\"}}]}\n\n")
		// 不再写入，模拟上游吐完部分内容后挂死。
	}()
	defer pw.Close()

	got, err := parseSSE(pr, 100*time.Millisecond)
	if err == nil {
		t.Fatalf("expected idle timeout error, got content=%q", got)
	}
	if !strings.Contains(err.Error(), "空闲超时") {
		t.Fatalf("error should mention idle timeout, got: %v", err)
	}
	if got != "" {
		t.Fatalf("must not return truncated content, got %q", got)
	}
}

func TestCompleteSSEIdleTimeoutTriggersOnKeepaliveOnlyStream(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		if n == 1 {
			// 首次请求只持续下发 ": keepalive" 心跳，不发送任何 data 行，
			// 模拟上游挂死但网关仍在保活：心跳不得续期，必须靠空闲超时中断并重试。
			for {
				io.WriteString(w, ": keepalive\n\n")
				if f != nil {
					f.Flush()
				}
				select {
				case <-r.Context().Done():
					return
				case <-time.After(25 * time.Millisecond):
				}
			}
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	o := &OpenAI{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client(), StreamIdleTimeout: 120 * time.Millisecond}
	got, err := o.Complete(context.Background(), Request{Model: "m", System: "s", User: "u"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "recovered" {
		t.Fatalf("content = %q, want %q", got, "recovered")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestParseSSEIdleTimeoutResetByReasoningData(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		w := func(s string) { io.WriteString(pw, s) }
		// 在超过 idle 的总时长里，以小于 idle 的间隔持续发送只含 reasoning_content 的 data 行，
		// 随后发送 content 与 [DONE]，验证思考流不会误触发空闲超时且能完整返回 content。
		w("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking 1\"}}]}\n\n")
		time.Sleep(40 * time.Millisecond)
		w("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking 2\"}}]}\n\n")
		time.Sleep(40 * time.Millisecond)
		w("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking 3\"}}]}\n\n")
		time.Sleep(40 * time.Millisecond)
		w("data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n")
		time.Sleep(40 * time.Millisecond)
		w("data: [DONE]\n\n")
	}()

	got, err := parseSSE(pr, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("reasoning-only data 不应触发空闲超时: %v", err)
	}
	if got != "done" {
		t.Fatalf("content = %q, want %q", got, "done")
	}
}
