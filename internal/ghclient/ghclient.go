// Package ghclient 是对用到的几个 GitHub REST 接口的轻量封装。
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/brian-devis/pr-reviewer-action/internal/httpx"
)

type Client struct {
	Token string
	API   string // 形如 https://api.github.com，不带结尾斜杠
	Repo  string // owner/name
	HTTP  *http.Client
}

// ReviewComment 是提交到 PR review 里的一条行内评论。
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// ExistingComment 是 PR 上已有的行内评论。
type ExistingComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Body string `json:"body"`
}

const (
	acceptJSON = "application/vnd.github+json"
	acceptDiff = "application/vnd.github.v3.diff"
	maxPages   = 10
)

func (c *Client) do(ctx context.Context, method, path, accept string, payload any) ([]byte, error) {
	var b []byte
	if payload != nil {
		var err error
		if b, err = json.Marshal(payload); err != nil {
			return nil, err
		}
	}
	return httpx.Do(ctx, c.HTTP, func() (*http.Request, error) {
		var body io.Reader
		if b != nil {
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		if b != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
}

// GetDiff 返回 PR 的 unified diff。
func (c *Client) GetDiff(ctx context.Context, pr int) (string, error) {
	data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", c.Repo, pr), acceptDiff, nil)
	return string(data), err
}

// UpsertComment 更新带 marker 的已有评论，没有则新建，避免每次 push 都刷一条。
func (c *Client) UpsertComment(ctx context.Context, pr int, marker, body string) error {
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", c.Repo, pr, page)
		data, err := c.do(ctx, http.MethodGet, path, acceptJSON, nil)
		if err != nil {
			return err
		}
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(data, &comments); err != nil {
			return err
		}
		for _, cm := range comments {
			if strings.Contains(cm.Body, marker) {
				_, err := c.do(ctx, http.MethodPatch,
					fmt.Sprintf("/repos/%s/issues/comments/%d", c.Repo, cm.ID), acceptJSON, map[string]string{"body": body})
				return err
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", c.Repo, pr), acceptJSON,
		map[string]string{"body": body})
	return err
}

// ListReviewComments 返回 PR 上已有的行内评论，用于去重。
func (c *Client) ListReviewComments(ctx context.Context, pr int) ([]ExistingComment, error) {
	var all []ExistingComment
	for page := 1; page <= maxPages; page++ {
		path := fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=100&page=%d", c.Repo, pr, page)
		data, err := c.do(ctx, http.MethodGet, path, acceptJSON, nil)
		if err != nil {
			return nil, err
		}
		var batch []ExistingComment
		if err := json.Unmarshal(data, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < 100 {
			break
		}
	}
	return all, nil
}

// CreateStatus 给指定提交设置 commit status。GitHub 对描述有两个限制，否则返回 422：
// 不超过 140 个字符，且不接受 4 字节 Unicode（如 emoji）。这里统一处理。
// 同一 context 重复设置会覆盖之前的状态。
//
// targetURL 可以为空；不为空时状态旁边会出现一个指向它的链接，用来跳转到执行日志。
func (c *Client) CreateStatus(ctx context.Context, sha, state, statusContext, description, targetURL string) error {
	description = strings.Map(func(r rune) rune {
		if r > 0xFFFF {
			return -1
		}
		return r
	}, description)
	if r := []rune(description); len(r) > 140 {
		description = string(r[:139]) + "…"
	}
	payload := map[string]string{"state": state, "context": statusContext, "description": description}
	if targetURL != "" {
		payload["target_url"] = targetURL
	}
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/statuses/%s", c.Repo, sha), acceptJSON, payload)
	return err
}

// CreateReview 一次性提交一个包含多条行内评论的 review。commitSHA 可为空，
// 为空时 GitHub 使用 PR 的最新提交。
func (c *Client) CreateReview(ctx context.Context, pr int, commitSHA, body string, comments []ReviewComment) error {
	payload := map[string]any{
		"event":    "COMMENT",
		"body":     body,
		"comments": comments,
	}
	if commitSHA != "" {
		payload["commit_id"] = commitSHA
	}
	_, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/pulls/%d/reviews", c.Repo, pr), acceptJSON, payload)
	return err
}
