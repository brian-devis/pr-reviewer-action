package review

import (
	"strings"
	"testing"
	"time"
)

func passedSandbox() *Sandbox {
	return &Sandbox{
		Image: "golang:1.22", Memory: "1g", CPUs: "1", Network: "none", Elapsed: 12400 * time.Millisecond,
		Steps: []SandboxStep{
			{Name: "test", Status: StepPassed, Detail: "通过", Duration: 8 * time.Second},
			{Name: "lint", Status: StepPassed, Detail: "通过", Duration: 4400 * time.Millisecond},
		},
	}
}

func TestRenderSandboxOnlyHasNoAIContent(t *testing.T) {
	got := Render(Report{NoAI: true, Sandbox: passedSandbox()})
	for _, want := range []string{Marker, "PR Reviewer", "沙盒检查通过", "单元测试", "Lint", "golang:1.22", "1g", "网络 已禁用", "12.4s"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"model:", "AI Code Review", "统计", "未发现明显问题"} {
		if strings.Contains(got, bad) {
			t.Errorf("关闭 AI 时不应出现 %q:\n%s", bad, got)
		}
	}
}

func TestRenderBothSectionsInOneComment(t *testing.T) {
	got := Render(Report{Summaries: []string{"改动合理"}, Files: 2, Model: "m1", Sandbox: passedSandbox()})
	if strings.Count(got, Marker) != 1 {
		t.Fatalf("同一条评论里只能有一个标记:\n%s", got)
	}
	for _, want := range []string{"PR Reviewer", "改动合理", "model: m1", "沙盒测试"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "改动合理") > strings.Index(got, "沙盒测试") {
		t.Error("AI 部分应在沙盒部分之前")
	}
}

func TestRenderAIOnlyUnchanged(t *testing.T) {
	got := Render(Report{Summaries: []string{"ok"}, Files: 1, Model: "m"})
	if !strings.Contains(got, "AI Code Review") || strings.Contains(got, "沙盒") {
		t.Fatalf("只开 AI 时应保持原样:\n%s", got)
	}
}

func TestRenderSandboxFailureShowsSanitizedOutput(t *testing.T) {
	s := passedSandbox()
	s.Steps[0] = SandboxStep{
		Name: "test", Status: StepFailed, Detail: "退出码 1", Duration: time.Second,
		Output: "--- FAIL: TestX\n\x1b[31mred\x1b[0m cc @victim <!-- hidden --> " + Marker + "\n```\nbreak out\n",
	}
	got := Render(Report{NoAI: true, Sandbox: s})
	if !strings.Contains(got, "沙盒检查未通过") || !strings.Contains(got, "<details>") || !strings.Contains(got, "--- FAIL: TestX") {
		t.Fatalf("失败输出应折叠展示:\n%s", got)
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "@victim") || strings.Contains(got, "<!-- hidden") {
		t.Fatalf("输出里的颜色码、提及、HTML 注释应被清理:\n%s", got)
	}
	if strings.Count(got, Marker) != 1 {
		t.Fatalf("输出里伪造的标记应被删除:\n%s", got)
	}
	if !strings.Contains(got, "````") {
		t.Fatalf("输出里有 ``` 时围栏应更长，避免提前闭合:\n%s", got)
	}
}

func TestRenderSandboxPassedStepsHaveNoOutputBlock(t *testing.T) {
	s := passedSandbox()
	s.Steps[0].Output = "ok  	pkg	0.1s"
	if got := Render(Report{NoAI: true, Sandbox: s}); strings.Contains(got, "<details>") {
		t.Fatalf("通过的步骤不展开输出:\n%s", got)
	}
}

func TestRenderSandboxOutputIsBounded(t *testing.T) {
	s := passedSandbox()
	s.Steps[1] = SandboxStep{Name: "lint", Status: StepFailed, Output: strings.Repeat("x", 200000) + "TAIL-MARKER"}
	got := Render(Report{NoAI: true, Sandbox: s})
	if len(got) > 20000 {
		t.Fatalf("评论过长 (%d)，会超出 GitHub 的 65536 限制", len(got))
	}
	if !strings.Contains(got, "TAIL-MARKER") || !strings.Contains(got, "仅末尾部分") {
		t.Fatalf("应保留输出末尾并说明被截断:\n%s", got[len(got)-300:])
	}
}

func TestSandboxHeadline(t *testing.T) {
	p, f, e := SandboxStep{Status: StepPassed}, SandboxStep{Status: StepFailed}, SandboxStep{Status: StepError}
	tests := []struct {
		steps []SandboxStep
		want  string
	}{
		{[]SandboxStep{p, p}, "✅ 沙盒检查通过"},
		{[]SandboxStep{p, f}, "❌ 沙盒检查未通过"},
		{[]SandboxStep{f, e}, "⚠️ 沙盒执行异常"},
		{nil, "⚠️ 沙盒执行异常"},
	}
	for _, tt := range tests {
		if got := SandboxHeadline(tt.steps); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

func TestRenderSandboxEscapesTableCells(t *testing.T) {
	s := passedSandbox()
	s.Steps[0].Detail = "a | b\nc"
	got := Render(Report{NoAI: true, Sandbox: s})
	if !strings.Contains(got, `a \| b c`) {
		t.Fatalf("表格单元格应转义竖线并去掉换行:\n%s", got)
	}
}
