package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/config"
	"github.com/brian-devis/pr-reviewer-action/internal/diff"
	"github.com/brian-devis/pr-reviewer-action/internal/ghclient"
	"github.com/brian-devis/pr-reviewer-action/internal/llm"
	"github.com/brian-devis/pr-reviewer-action/internal/review"
)

func main() {
	if err := run(); err != nil {
		fmt.Printf("::error::%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// 防止密钥出现在后续日志里
	fmt.Printf("::add-mask::%s\n", cfg.APIKey)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	gh := &ghclient.Client{
		Token: cfg.GitHubToken,
		API:   cfg.GitHubAPI,
		Repo:  cfg.Repo,
		HTTP:  &http.Client{Timeout: 2 * time.Minute},
	}
	provider := newProvider(cfg, &http.Client{Timeout: 5 * time.Minute})

	// 状态检查失败（例如 workflow 没有 statuses: write 权限）不应该让审查本身失败
	setStatus := func(state, desc string) {
		if cfg.HeadSHA == "" {
			return
		}
		if err := gh.CreateStatus(ctx, cfg.HeadSHA, state, cfg.StatusContext, desc); err != nil {
			fmt.Printf("::warning::设置 commit status 失败（需要 statuses: write 权限）: %v\n", err)
		}
	}
	setStatus(review.StatePending, "AI 审查进行中")

	state, desc, err := execute(ctx, cfg, gh, provider)
	if err != nil {
		// 错误详情可能包含接口返回内容，不写进公开的状态描述里
		setStatus(review.StateError, "审查出错，详见 Actions 日志")
		return err
	}
	setStatus(state, desc)
	fmt.Printf("commit status: %s（%s）\n", state, desc)
	return nil
}

// execute 完成一次审查，返回应当设置的 commit status。
func execute(ctx context.Context, cfg *config.Config, gh *ghclient.Client, provider llm.Provider) (state, desc string, err error) {
	raw, err := gh.GetDiff(ctx, cfg.PRNumber)
	if err != nil {
		return "", "", fmt.Errorf("获取 diff 失败: %w", err)
	}
	files := diff.Select(diff.Parse(raw), cfg.Exclude)
	if len(files) == 0 {
		fmt.Println("没有需要审查的文件，跳过")
		return review.StateSuccess, "没有需要审查的文件", nil
	}
	byPath := make(map[string]*diff.File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}

	chunks, skipped := diff.Chunk(files, cfg.MaxDiffBytes, cfg.MaxRequests)

	report := review.Report{Files: len(files) - len(skipped), Model: cfg.Model}
	var all []review.Comment
	failures, unparsed := 0, 0
	for i, chunk := range chunks {
		res, text, ok, err := reviewChunk(ctx, provider, cfg.Model, chunk)
		if err != nil {
			failures++
			fmt.Printf("::warning::第 %d/%d 批请求失败: %v\n", i+1, len(chunks), err)
			report.Notes = append(report.Notes, fmt.Sprintf("第 %d/%d 批文件的模型请求失败，这部分未被审查。", i+1, len(chunks)))
			continue
		}
		if !ok {
			// 解析不出结构化结果，就无法确认这批文件没有问题
			unparsed++
			res = review.Result{Summary: text}
			report.Notes = append(report.Notes, "模型没有返回合法 JSON，已按原文展示，无法生成行内评论。")
		}
		report.Summaries = append(report.Summaries, res.Summary)
		all = append(all, res.Comments...)
	}
	if failures == len(chunks) {
		return "", "", errors.New("所有模型请求均失败")
	}

	for _, f := range files {
		if f.Truncated {
			report.Notes = append(report.Notes, fmt.Sprintf("`%s` 过大，已截断后审查。", f.Path))
		}
	}
	if len(skipped) > 0 {
		names := make([]string, len(skipped))
		for i, f := range skipped {
			names[i] = "`" + f.Path + "`"
		}
		report.Notes = append(report.Notes, fmt.Sprintf(
			"超过单个 PR 的请求上限（max_requests=%d），以下 %d 个文件未审查：%s",
			cfg.MaxRequests, len(skipped), strings.Join(names, "、")))
	}

	// 先校验再截断：行号无效的评论不应该挤掉有效的高严重度评论
	inline, orphans := review.Validate(all, byPath)
	// 统计本次模型发现的全部问题，包括稍后会被去重跳过、不再重复发布的：
	// 状态要反映“当前代码还有没有问题”，而不是“这次新发了多少条评论”。
	counts := review.Count(inline, orphans)
	inline, droppedInline := review.Cap(inline, review.MaxComments)
	orphans, droppedOrphans := review.Cap(orphans, review.MaxComments)
	if dropped := droppedInline + droppedOrphans; dropped > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"模型给出的评论过多，已按严重度保留最重要的部分，另有 %d 条被省略。", dropped))
	}
	if err := postInline(ctx, gh, cfg, inline); err != nil {
		// 行内评论提交失败（例如 push 导致行号变化返回 422），退回到总评里展示
		fmt.Printf("::warning::提交行内评论失败，改为写入总评: %v\n", err)
		orphans = append(orphans, inline...)
		inline = nil
		report.Notes = append(report.Notes, "行内评论提交失败，相关问题已列在上方。")
	}
	report.Inline, report.Orphans = inline, orphans

	if err := gh.UpsertComment(ctx, cfg.PRNumber, review.Marker, review.Render(report)); err != nil {
		return "", "", fmt.Errorf("发布评论失败: %w", err)
	}
	fmt.Printf("审查完成：%d 条行内评论，%d 条总评内问题\n", len(inline), len(orphans))

	incomplete := failures > 0 || unparsed > 0 || len(skipped) > 0
	state, desc = review.Decide(counts, cfg.FailOn, incomplete)
	return state, desc, nil
}

// maxParseAttempts 是模型返回无法解析的 JSON 时，同一批文件最多请求的次数。
// 模型偶尔会在字符串里写出未转义的引号，重新生成通常就正常了。
const maxParseAttempts = 2

// reviewChunk 审查一批文件。请求失败直接返回错误；返回内容解析不出结构化结果时重试，
// 仍然失败则 ok 为 false，并把最后一次的原文放在 text 里。
func reviewChunk(ctx context.Context, p llm.Provider, model string, chunk []*diff.File) (res review.Result, text string, ok bool, err error) {
	system, user := review.Prompt(chunk)
	for attempt := 1; attempt <= maxParseAttempts; attempt++ {
		text, err = p.Complete(ctx, llm.Request{Model: model, System: system, User: user, JSON: true})
		if err != nil {
			return review.Result{}, "", false, err
		}
		if res, ok = review.Parse(text); ok {
			return res, text, true, nil
		}
		fmt.Printf("::warning::模型返回的内容无法解析为 JSON（第 %d/%d 次）\n", attempt, maxParseAttempts)
	}
	return review.Result{}, text, false, nil
}

// newProvider 按 api_format 选择模型协议。config.Load 已经保证 format 合法。
func newProvider(cfg *config.Config, client *http.Client) llm.Provider {
	if cfg.APIFormat == "anthropic" {
		return &llm.Anthropic{BaseURL: cfg.APIBaseURL, APIKey: cfg.APIKey, HTTP: client}
	}
	return &llm.OpenAI{BaseURL: cfg.APIBaseURL, APIKey: cfg.APIKey, HTTP: client}
}

// postInline 提交行内评论，已经存在的相同评论会被跳过。
func postInline(ctx context.Context, gh *ghclient.Client, cfg *config.Config, inline []review.Comment) error {
	if len(inline) == 0 {
		return nil
	}
	existing, err := gh.ListReviewComments(ctx, cfg.PRNumber)
	if err != nil {
		fmt.Printf("::warning::读取已有行内评论失败，可能出现重复: %v\n", err)
	}
	fresh := filterFresh(inline, existing)
	if len(fresh) == 0 {
		return nil
	}
	return gh.CreateReview(ctx, cfg.PRNumber, cfg.HeadSHA,
		fmt.Sprintf("🤖 AI 行内审查：%d 条评论", len(fresh)), fresh)
}

// maxBotPerLocation 限制同一行上机器人评论的总数，防止措辞反复变化造成堆积。
const maxBotPerLocation = 3

// filterFresh 去掉重复的行内评论，转成可提交的格式。
// 同一位置上，只有当已有的机器人评论都在说别的事时才会追加新评论：
// 模型每次运行措辞都会变，所以比较的是内容相似度，而不是原文或仅仅是行号。
func filterFresh(inline []review.Comment, existing []ghclient.ExistingComment) []ghclient.ReviewComment {
	posted := map[string][]string{}
	for _, e := range existing {
		if strings.Contains(e.Body, review.Marker) {
			k := locKey(e.Path, e.Line)
			posted[k] = append(posted[k], e.Body)
		}
	}
	var fresh []ghclient.ReviewComment
	for _, c := range inline {
		k := locKey(c.File, int(c.Line))
		body := review.RenderInline(c)
		if len(posted[k]) >= maxBotPerLocation || anySimilar(posted[k], body) {
			continue
		}
		posted[k] = append(posted[k], body)
		fresh = append(fresh, ghclient.ReviewComment{
			Path: c.File, Line: int(c.Line), Side: "RIGHT", Body: body,
		})
	}
	return fresh
}

func anySimilar(bodies []string, body string) bool {
	for _, b := range bodies {
		if review.Similar(b, body) {
			return true
		}
	}
	return false
}

func locKey(path string, line int) string {
	return fmt.Sprintf("%s:%d", path, line)
}
