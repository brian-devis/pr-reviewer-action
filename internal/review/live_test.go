//go:build live

// 这些测试会真的调用模型，用来验证 prompt 对已知误报的抑制效果。默认跳过：
//
//	PRR_LIVE=1 \
//	PRR_LIVE_BASE_URL=https://api.deepseek.com/v1 PRR_LIVE_FORMAT=openai \
//	PRR_LIVE_API_KEY=... PRR_LIVE_MODEL=deepseek-v4-pro \
//	go test -tags live -v -run TestLiveFalsePositives ./internal/review/
//
// 模型输出有随机性，单个用例失败时应重跑几次再判断，不要把一次失败当成规则失效。
package review

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/diff"
	"github.com/brian-devis/pr-reviewer-action/internal/llm"
)

func liveProvider(t *testing.T) llm.Provider {
	t.Helper()
	if os.Getenv("PRR_LIVE") != "1" {
		t.Skip("设置 PRR_LIVE=1 才会真的调用模型")
	}
	base := strings.TrimRight(os.Getenv("PRR_LIVE_BASE_URL"), "/")
	key := os.Getenv("PRR_LIVE_API_KEY")
	model := os.Getenv("PRR_LIVE_MODEL")
	if base == "" || key == "" || model == "" {
		t.Skip("需要 PRR_LIVE_BASE_URL、PRR_LIVE_API_KEY、PRR_LIVE_MODEL")
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	if strings.EqualFold(os.Getenv("PRR_LIVE_FORMAT"), "anthropic") {
		return &llm.Anthropic{BaseURL: base, APIKey: key, HTTP: client}
	}
	return &llm.OpenAI{BaseURL: base, APIKey: key, HTTP: client}
}

// liveReview 走和线上完全一样的路径：解析 diff → 构造 prompt → 调用模型 → 解析 JSON。
func liveReview(t *testing.T, p llm.Provider, raw string) []Comment {
	t.Helper()
	files := diff.Select(diff.Parse(raw), nil)
	if len(files) == 0 {
		t.Fatal("样本 diff 解析为空")
	}
	system, user := Prompt(files)
	model := os.Getenv("PRR_LIVE_MODEL")
	var lastErr error
	// 模型输出有随机性：请求失败或解析失败都重试一次，仍失败才判定为不通过
	for attempt := 0; attempt < 2; attempt++ {
		text, err := p.Complete(context.Background(), llm.Request{
			Model: model, System: system, User: user, JSON: true,
		})
		if err != nil {
			lastErr = err
			t.Logf("第 %d 次调用失败: %v", attempt+1, err)
			continue
		}
		if res, ok := Parse(text); ok {
			return res.Comments
		}
		if len(text) > 300 {
			text = text[:300] + "…"
		}
		lastErr = fmt.Errorf("无法解析为 JSON，原文开头: %s", text)
	}
	t.Fatalf("模型调用失败: %v", lastErr)
	return nil
}

// 反馈 1：if 与 runs-on 本来就在同一缩进，却被报成 error 并阻断合并。
const liveIndentDiff = `diff --git a/.github/workflows/pr-review.yml b/.github/workflows/pr-review.yml
index 1111111..2222222 100644
--- a/.github/workflows/pr-review.yml
+++ b/.github/workflows/pr-review.yml
@@ -15,6 +15,7 @@
 jobs:
   review:
+    if: github.event.pull_request.head.repo.full_name == github.repository
     runs-on: ubuntu-latest
     steps:
-      - uses: brian-devis/pr-reviewer-action@v0
+      - uses: brian-devis/pr-reviewer-action@v0.6.0
       - run: echo ok
`

func TestLiveFalsePositive_AlignedIndentIsNotAnError(t *testing.T) {
	p := liveProvider(t)
	for _, c := range liveReview(t, p, liveIndentDiff) {
		if c.Severity != "error" {
			continue
		}
		msg := c.Message + " " + c.Suggestion
		for _, kw := range []string{"缩进", "对齐", "indent"} {
			if strings.Contains(msg, kw) {
				t.Fatalf("同级缩进被误报为 error：%s", msg)
			}
		}
	}
}

// 反馈 2：模型建议改用 pull_request_target，与项目安全要求冲突。
const liveForkDiff = `diff --git a/.github/workflows/review.yml b/.github/workflows/review.yml
index 1111111..2222222 100644
--- a/.github/workflows/review.yml
+++ b/.github/workflows/review.yml
@@ -15,6 +15,7 @@
 jobs:
   review:
+    if: github.event.pull_request.head.repo.full_name == github.repository
     runs-on: ubuntu-latest
     steps:
       - uses: actions/checkout@v4
       - run: echo ok
`

func TestLiveFalsePositive_NeverSuggestsPullRequestTarget(t *testing.T) {
	p := liveProvider(t)
	for _, c := range liveReview(t, p, liveForkDiff) {
		text := c.Message + " " + c.Suggestion
		if !strings.Contains(text, "pull_request_target") {
			continue
		}
		// 提到这个词本身不算错：明确劝阻是正确做法。只有“建议使用”才算危险建议。
		warned := false
		for _, neg := range []string{"不要", "禁止", "避免", "不应", "不能", "切勿"} {
			if strings.Contains(text, neg) {
				warned = true
			}
		}
		if !warned {
			t.Fatalf("出现了危险的 pull_request_target 建议：%s", text)
		}
	}
}

// 反馈 3：只开沙盒时，模型把失败归因于缺少 api_key。
const liveSandboxOnlyDiff = `diff --git a/.github/workflows/review.yml b/.github/workflows/review.yml
index 1111111..2222222 100644
--- a/.github/workflows/review.yml
+++ b/.github/workflows/review.yml
@@ -6,6 +6,12 @@ jobs:
     steps:
       - uses: actions/checkout@v4
+      - uses: brian-devis/pr-reviewer-action@v0.6.0
+        with:
+          github_token: ${{ secrets.GITHUB_TOKEN }}
+          enable_ai_review: "false"
+          enable_sandbox: "true"
+          sandbox_test_command: go test ./...
+          sandbox_lint_command: go vet ./...
`

func TestLiveFalsePositive_SandboxOnlyDoesNotRequireAPIKey(t *testing.T) {
	p := liveProvider(t)
	for _, c := range liveReview(t, p, liveSandboxOnlyDiff) {
		if strings.Contains(c.Message+c.Suggestion, "api_key") {
			t.Fatalf("只开沙盒时不应质疑 api_key：%s", c.Message)
		}
	}
}
