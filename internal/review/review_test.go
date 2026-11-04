package review

import (
	"regexp"
	"strings"
	"testing"

	"github.com/brian-devis/pr-reviewer-action/internal/diff"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantOK  bool
		wantLen int
		line    int
	}{
		{"纯 JSON", `{"summary":"s","comments":[{"file":"a.go","line":3,"severity":"error","message":"m"}]}`, true, 1, 3},
		{"代码围栏", "```json\n{\"summary\":\"s\",\"comments\":[{\"file\":\"a.go\",\"line\":5,\"message\":\"m\"}]}\n```", true, 1, 5},
		{"字符串行号", `{"summary":"s","comments":[{"file":"a.go","line":"L7","message":"m"}]}`, true, 1, 7},
		{"前后有多余文字", `好的：{"summary":"s","comments":[]} 以上`, true, 0, 0},
		{"非 JSON", `这段代码看起来没问题`, false, 0, 0},
		{"坏 JSON", `{"summary": `, false, 0, 0},
		{"行号无效只影响该评论", `{"summary":"s","comments":[{"file":"a","line":"abc","message":"m"},{"file":"b","line":4,"message":"n"}]}`, true, 2, 0},
		{"行号为 null", `{"summary":"s","comments":[{"file":"a","line":null,"message":"m"}]}`, true, 1, 0},
		{"后面追加带花括号的说明", `{"summary":"s","comments":[]} 注意 {x} 的写法`, true, 0, 0},
		{"多个对象取第一个", `{"summary":"first","comments":[{"file":"a","line":2,"message":"m"}]}{"summary":"second","comments":[]}`, true, 1, 2},
		{"前面有带花括号的说明", `示例 {格式} 如下：{"summary":"s","comments":[{"file":"a","line":6,"message":"m"}]}`, true, 1, 6},
		{"无关的 JSON 对象", `{"foo":1}`, false, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, ok := Parse(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if len(r.Comments) != tt.wantLen {
				t.Fatalf("comments = %d, want %d", len(r.Comments), tt.wantLen)
			}
			if tt.wantLen > 0 && int(r.Comments[0].Line) != tt.line {
				t.Fatalf("line = %d, want %d", r.Comments[0].Line, tt.line)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	files := map[string]*diff.File{
		"a.go": {Path: "a.go", Lines: map[int]bool{2: true, 3: true}},
	}
	in := []Comment{
		{File: "a.go", Line: 2, Severity: "ERROR", Message: "有效"},
		{File: "./a.go", Line: 3, Severity: "weird", Message: "路径带前缀、严重度未知"},
		{File: "a.go", Line: 99, Message: "行号不在 diff"},
		{File: "x.go", Line: 1, Message: "文件不在 diff"},
		{File: "a.go", Line: 2, Severity: "error", Message: "有效"}, // 重复
		{File: "a.go", Line: 2, Message: "   "},                   // 空消息
	}
	inline, orphan := Validate(in, files)
	if len(inline) != 2 || len(orphan) != 2 {
		t.Fatalf("inline=%d orphan=%d, want 2/2", len(inline), len(orphan))
	}
	if inline[0].Severity != "error" || inline[1].Severity != "warning" {
		t.Fatalf("严重度归一化错误: %q %q", inline[0].Severity, inline[1].Severity)
	}
}

func TestRenderInline(t *testing.T) {
	got := RenderInline(Comment{Severity: "error", Message: "SQL 注入", Suggestion: "使用参数化查询"})
	for _, want := range []string{"🔴", "SQL 注入", "使用参数化查询", Marker} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
	if strings.Contains(RenderInline(Comment{Severity: "info", Message: "m"}), "建议") {
		t.Error("没有 suggestion 时不应出现建议段落")
	}
}

func TestRender(t *testing.T) {
	got := Render(Report{
		Summaries: []string{"改动合理"},
		Files:     2,
		Inline:    []Comment{{Severity: "error", Message: "a"}},
		Orphans:   []Comment{{File: "b.go", Line: 9, Severity: "info", Message: "多行\n说明"}},
		Notes:     []string{"已截断"},
		Model:     "m1",
	})
	for _, want := range []string{Marker, "改动合理", "审查 2 个文件", "发现 2 个问题", "🔴 1", "🔵 1", "`b.go:9`", "已截断", "model: m1"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
}

func TestRenderNoIssues(t *testing.T) {
	got := Render(Report{Files: 1, Model: "m"})
	if !strings.Contains(got, "未发现明显问题") {
		t.Fatalf("应提示无问题:\n%s", got)
	}
}

func TestPrompt(t *testing.T) {
	sys, user := Prompt([]*diff.File{{Text: "### FILE: a.go\nL1 + x\n"}})
	if !strings.Contains(sys, "JSON") || !strings.Contains(user, "L1 + x") {
		t.Fatalf("prompt 异常:\n%s\n%s", sys, user)
	}
	m := regexp.MustCompile(`<(diff-[0-9a-f]{16})>`).FindStringSubmatch(user)
	if m == nil {
		t.Fatalf("用户消息里应有随机标签:\n%s", user)
	}
	tag := m[1]
	if !strings.Contains(user, "</"+tag+">") || !strings.Contains(sys, "<"+tag+">") || strings.Contains(sys, "{{TAG}}") {
		t.Fatalf("标签在 system 和 user 中不一致:\n%s\n%s", sys, user)
	}
	if _, user2 := Prompt(nil); strings.Contains(user2, tag) {
		t.Fatal("每次调用应使用不同的随机标签")
	}
}

func TestValidateKeepsRealBDirectory(t *testing.T) {
	files := map[string]*diff.File{
		"b/foo.go": {Path: "b/foo.go", Lines: map[int]bool{1: true}},
		"a.go":     {Path: "a.go", Lines: map[int]bool{1: true}},
	}
	inline, orphan := Validate([]Comment{
		{File: "b/foo.go", Line: 1, Message: "真实的 b 目录"},
		{File: "b/a.go", Line: 1, Message: "带了 diff 头部的 b/ 前缀"},
	}, files)
	if len(orphan) != 0 || len(inline) != 2 {
		t.Fatalf("inline=%d orphan=%d, want 2/0", len(inline), len(orphan))
	}
	if inline[0].File != "b/foo.go" || inline[1].File != "a.go" {
		t.Fatalf("路径归一化错误: %q %q", inline[0].File, inline[1].File)
	}
}

func TestRenderOrphanWithoutLine(t *testing.T) {
	got := Render(Report{Orphans: []Comment{{File: "x.go", Line: 0, Severity: "warning", Message: "m"}}, Model: "m"})
	if !strings.Contains(got, "`x.go`") || strings.Contains(got, "x.go:0") {
		t.Fatalf("行号为 0 时不应显示 :0:\n%s", got)
	}
}
