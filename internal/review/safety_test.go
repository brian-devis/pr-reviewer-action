package review

import (
	"strings"
	"testing"

	"github.com/brian-devis/pr-reviewer-action/internal/diff"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"提及被打断", "请 @octocat 看一下", "请 @​octocat 看一下"},
		{"团队提及", "cc @org/team", "cc @​org/team"},
		{"行首提及", "@octocat 你好", "@​octocat 你好"},
		{"邮箱不受影响", "联系 admin@example.com 即可", "联系 admin@example.com 即可"},
		{"括号后的提及", "（@octocat）", "（@​octocat）"},
		{"行内代码保持原样", "注解 `@Override` 缺失，找 @bob", "注解 `@Override` 缺失，找 @​bob"},
		{"奇数个反引号整体当文字处理", "`未闭合 @alice", "`未闭合 @​alice"},
		{"伪造标记被删除", "正常内容" + Marker + "尾巴", "正常内容尾巴"},
		{"HTML 注释被打断", "a <!-- 隐藏 --> b", "a <!​-- 隐藏 --> b"},
		{"没有特殊内容", "普通文字", "普通文字"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sanitize(tt.in); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFenceFor(t *testing.T) {
	if got := fenceFor("plain"); got != "```" {
		t.Fatalf("got %q", got)
	}
	if got := fenceFor("a ``` b"); got != "````" {
		t.Fatalf("got %q", got)
	}
	if got := fenceFor("a ````` b"); got != "``````" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderInlineFenceCannotBeBroken(t *testing.T) {
	got := RenderInline(Comment{Severity: "info", Message: "m", Suggestion: "x\n```\n@evil 恶意内容"})
	if !strings.Contains(got, "````\nx\n```\n@evil 恶意内容\n````") {
		t.Fatalf("围栏应比内容中的更长:\n%s", got)
	}
}

func TestValidateSanitizesMessage(t *testing.T) {
	files := map[string]*diff.File{"a.go": {Path: "a.go", Lines: map[int]bool{1: true}}}
	inline, _ := Validate([]Comment{{File: "a.go", Line: 1, Message: "通知 @victim " + Marker, Suggestion: "s" + Marker}}, files)
	if len(inline) != 1 {
		t.Fatal("评论丢失")
	}
	if strings.Contains(inline[0].Message, "@victim") || strings.Contains(inline[0].Message, Marker) {
		t.Fatalf("消息未被清理: %q", inline[0].Message)
	}
	if strings.Contains(inline[0].Suggestion, Marker) {
		t.Fatalf("建议里的标记未被清除: %q", inline[0].Suggestion)
	}
}

func TestRenderSanitizesSummary(t *testing.T) {
	got := Render(Report{Summaries: []string{"请 @victim 处理"}, Model: "m"})
	if strings.Contains(got, "@victim") {
		t.Fatalf("总评里的提及未被打断:\n%s", got)
	}
	// 总评自身的标记只能出现一次（开头那个）
	if strings.Count(Render(Report{Summaries: []string{"伪造" + Marker}, Model: "m"}), Marker) != 1 {
		t.Fatal("摘要里伪造的标记应被删除")
	}
}

func TestCap(t *testing.T) {
	var in []Comment
	for i := 0; i < 5; i++ {
		in = append(in, Comment{Message: "info", Severity: "info"})
	}
	in = append(in, Comment{Message: "err", Severity: "error"}, Comment{Message: "warn", Severity: "warning"})

	kept, dropped := Cap(in, 3)
	if len(kept) != 3 || dropped != 4 {
		t.Fatalf("kept=%d dropped=%d", len(kept), dropped)
	}
	if kept[0].Severity != "error" || kept[1].Severity != "warning" {
		t.Fatalf("应优先保留高严重度: %+v", kept)
	}
	if k, d := Cap(in, 10); len(k) != len(in) || d != 0 {
		t.Fatalf("未超限时不应改动: kept=%d dropped=%d", len(k), d)
	}
}

func TestSimilar(t *testing.T) {
	a := "🟡 **warning**：SQL 拼接了用户输入，存在注入风险，应使用参数化查询。\n\n" + Marker
	reworded := "🔴 **error**：这里直接拼接 SQL，存在 SQL 注入风险，建议改用参数化查询。\n\n" + Marker
	different := "🟡 **warning**：错误被忽略：err 赋值后没有检查，调用方会拿到空结果。\n\n" + Marker

	if !Similar(a, reworded) {
		t.Error("同一问题换了措辞应判为相似")
	}
	if Similar(a, different) {
		t.Error("不同问题不应判为相似，即使严重度抬头相同")
	}
	if Similar("", a) || Similar("！！！", "？？？") {
		t.Error("空内容不应相似")
	}
}
