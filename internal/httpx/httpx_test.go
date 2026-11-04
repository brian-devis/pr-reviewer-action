package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newReq(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) { return http.NewRequest(http.MethodGet, url, nil) }
}

func TestDoRetriesOnServerError(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	data, err := Do(context.Background(), srv.Client(), newReq(srv.URL))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "ok" || calls != 3 {
		t.Fatalf("data=%q calls=%d", data, calls)
	}
}

func TestDoDoesNotRetryClientError(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := Do(context.Background(), srv.Client(), newReq(srv.URL))
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("want 400 StatusError, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestDoGivesUpAfterMaxAttempts(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if _, err := Do(context.Background(), srv.Client(), newReq(srv.URL)); err == nil {
		t.Fatal("expected error")
	}
	if calls != maxAttempts {
		t.Fatalf("calls = %d, want %d", calls, maxAttempts)
	}
}

func postReq(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) { return http.NewRequest(http.MethodPost, url, nil) }
}

func TestDoDoesNotRetryPostOnServerError(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := Do(context.Background(), srv.Client(), postReq(srv.URL)); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("POST 遇到 5xx 不应重试，calls = %d", calls)
	}
}

func TestDoRetriesPostOnRateLimit(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	data, err := Do(context.Background(), srv.Client(), postReq(srv.URL))
	if err != nil || string(data) != "ok" || calls != 2 {
		t.Fatalf("data=%q err=%v calls=%d", data, err, calls)
	}
}

func TestDoRetryUnsafeRetriesPostOnServerError(t *testing.T) {
	baseDelay = time.Millisecond
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			http.Error(w, "boom", http.StatusBadGateway)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	data, err := Do(context.Background(), srv.Client(), postReq(srv.URL), RetryUnsafe())
	if err != nil || string(data) != "ok" || calls != 3 {
		t.Fatalf("data=%q err=%v calls=%d", data, err, calls)
	}
}
