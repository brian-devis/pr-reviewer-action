package ghclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recorded struct {
	method, path, body string
}

func newServer(t *testing.T, handler func(r *http.Request, body string) (int, string)) (*Client, *[]recorded) {
	t.Helper()
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, recorded{r.Method, r.URL.Path, string(b)})
		code, resp := handler(r, string(b))
		w.WriteHeader(code)
		io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return &Client{Token: "tok", API: srv.URL, Repo: "o/r", HTTP: srv.Client()}, &reqs
}

func TestGetDiffUsesDiffAccept(t *testing.T) {
	var accept, auth string
	c, _ := newServer(t, func(r *http.Request, _ string) (int, string) {
		accept, auth = r.Header.Get("Accept"), r.Header.Get("Authorization")
		return 200, "diff --git a/x b/x"
	})
	got, err := c.GetDiff(context.Background(), 3)
	if err != nil || got != "diff --git a/x b/x" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if accept != acceptDiff || auth != "Bearer tok" {
		t.Fatalf("accept=%q auth=%q", accept, auth)
	}
}

func TestUpsertCommentUpdatesExisting(t *testing.T) {
	c, reqs := newServer(t, func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet {
			return 200, `[{"id":1,"body":"other"},{"id":7,"body":"old <!-- m -->"}]`
		}
		return 200, `{}`
	})
	if err := c.UpsertComment(context.Background(), 1, "<!-- m -->", "new <!-- m -->"); err != nil {
		t.Fatal(err)
	}
	last := (*reqs)[len(*reqs)-1]
	if last.method != http.MethodPatch || last.path != "/repos/o/r/issues/comments/7" {
		t.Fatalf("应 PATCH 评论 7，实际 %s %s", last.method, last.path)
	}
}

func TestUpsertCommentCreatesWhenMissing(t *testing.T) {
	c, reqs := newServer(t, func(r *http.Request, _ string) (int, string) {
		if r.Method == http.MethodGet {
			return 200, `[{"id":1,"body":"other"}]`
		}
		return 201, `{}`
	})
	if err := c.UpsertComment(context.Background(), 1, "<!-- m -->", "body"); err != nil {
		t.Fatal(err)
	}
	last := (*reqs)[len(*reqs)-1]
	if last.method != http.MethodPost || last.path != "/repos/o/r/issues/1/comments" {
		t.Fatalf("应 POST 新评论，实际 %s %s", last.method, last.path)
	}
}

func TestCreateReviewPayload(t *testing.T) {
	c, reqs := newServer(t, func(*http.Request, string) (int, string) { return 200, `{}` })
	err := c.CreateReview(context.Background(), 5, "abc", "summary", []ReviewComment{
		{Path: "a.go", Line: 3, Side: "RIGHT", Body: "bug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := (*reqs)[0]
	if req.path != "/repos/o/r/pulls/5/reviews" {
		t.Fatalf("path = %s", req.path)
	}
	var p struct {
		Event    string          `json:"event"`
		CommitID string          `json:"commit_id"`
		Comments []ReviewComment `json:"comments"`
	}
	if err := json.Unmarshal([]byte(req.body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "COMMENT" || p.CommitID != "abc" || len(p.Comments) != 1 || p.Comments[0].Line != 3 {
		t.Fatalf("payload 异常: %s", req.body)
	}
}

func TestCreateReviewOmitsEmptyCommit(t *testing.T) {
	c, reqs := newServer(t, func(*http.Request, string) (int, string) { return 200, `{}` })
	if err := c.CreateReview(context.Background(), 5, "", "s", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*reqs)[0].body, "commit_id") {
		t.Fatalf("空 commit 不应出现在 payload: %s", (*reqs)[0].body)
	}
}

func TestListReviewComments(t *testing.T) {
	c, _ := newServer(t, func(*http.Request, string) (int, string) {
		return 200, `[{"path":"a.go","line":3,"body":"x"},{"path":"b.go","line":null,"body":"y"}]`
	})
	got, err := c.ListReviewComments(context.Background(), 1)
	if err != nil || len(got) != 2 || got[0].Path != "a.go" || got[1].Line != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
