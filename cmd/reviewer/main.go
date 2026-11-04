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
	provider := &llm.OpenAI{
		BaseURL: cfg.APIBaseURL,
		APIKey:  cfg.APIKey,
		HTTP:    &http.Client{Timeout: 5 * time.Minute},
	}

	raw, err := gh.GetDiff(ctx, cfg.PRNumber)
	if err != nil {
		return fmt.Errorf("获取 diff 失败: %w", err)
	}
	files := diff.Select(diff.Parse(raw), cfg.Exclude)
	if len(files) == 0 {
		fmt.Println("没有需要审查的文件，跳过")
		return nil
	}
	byPath := make(map[string]*diff.File, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}

	chunks, skipped := diff.Chunk(files, cfg.MaxDiffBytes, cfg.MaxRequests)

	report := review.Report{Files: len(files) - len(skipped), Model: cfg.Model}
	var all []review.Comment
	failures := 0
	for i, chunk := range chunks {
		system, user := review.Prompt(chunk)
		text, err := provider.Complete(ctx, llm.Request{Model: cfg.Model, System: system, User: user, JSON: true})
		if err != nil {
			failures++
			fmt.Printf("::warning::第 %d/%d 批请求失败: %v\n", i+1, len(chunks), err)
			report.Notes = append(report.Notes, fmt.Sprintf("第 %d/%d 批文件的模型请求失败，这部分未被审查。", i+1, len(chunks)))
			continue
		}
		res, ok := review.Parse(text)
		if !ok {
			res = review.Result{Summary: text}
			report.Notes = append(report.Notes, "模型没有返回合法 JSON，已按原文展示，无法生成行内评论。")
		}
		report.Summaries = append(report.Summaries, res.Summary)
		all = append(all, res.Comments...)
	}
	if failures == len(chunks) {
		return errors.New("所有模型请求均失败")
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
		return fmt.Errorf("发布评论失败: %w", err)
	}
	fmt.Printf("审查完成：%d 条行内评论，%d 条总评内问题\n", len(inline), len(orphans))
	return nil
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
