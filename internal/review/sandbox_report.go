package review

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/textutil"
)

// 沙盒步骤的结论，与 sandbox.Status 的取值一致；这里不依赖 sandbox 包，
// 让评审和沙盒两个模块互不引用。
const (
	StepPassed = "passed"
	StepFailed = "failed"
	StepError  = "error"
)

// maxStepOutput 是总评里每个步骤最多展示的输出字节数。
// GitHub 评论上限是 65536 个字符，留足给 AI 部分。
const maxStepOutput = 6000

// SandboxStep 是沙盒里的一步检查（依赖准备、测试或 Lint）。
type SandboxStep struct {
	Name      string // deps | test | lint
	Status    string // StepPassed | StepFailed | StepError
	Detail    string
	Duration  time.Duration
	Output    string
	Truncated bool
}

// Sandbox 是总评里沙盒测试一节的内容。
type Sandbox struct {
	Kind    string // go | node | python | generic，用于展示识别出的项目类型
	Image   string
	Memory  string
	CPUs    string
	Network string // "none" 表示断网
	Elapsed time.Duration
	Steps   []SandboxStep
}

var stepTitles = map[string]string{"deps": "依赖准备", "test": "单元测试", "lint": "Lint / 静态检查"}

var kindLabels = map[string]string{"go": "Go", "node": "Node.js", "python": "Python", "generic": "自定义"}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func stepTitle(name string) string {
	if t, ok := stepTitles[name]; ok {
		return t
	}
	return name
}

func stepBadge(status string) string {
	switch status {
	case StepPassed:
		return "✅ 通过"
	case StepFailed:
		return "❌ 未通过"
	default:
		return "⚠️ 异常"
	}
}

// SandboxHeadline 返回沙盒整体结论，用作总评里这一节的标题。
func SandboxHeadline(steps []SandboxStep) string {
	overall := StepPassed
	for _, s := range steps {
		if s.Status == StepError {
			return "⚠️ 沙盒执行异常"
		}
		if s.Status == StepFailed {
			overall = StepFailed
		}
	}
	if overall == StepFailed {
		return "❌ 沙盒检查未通过"
	}
	if len(steps) == 0 {
		return "⚠️ 沙盒执行异常"
	}
	return "✅ 沙盒检查通过"
}

func renderSandbox(b *strings.Builder, s Sandbox) {
	fmt.Fprintf(b, "\n### 🧪 沙盒测试 · %s\n\n", SandboxHeadline(s.Steps))

	b.WriteString("| 检查 | 结果 | 说明 | 耗时 |\n|---|---|---|---|\n")
	for _, st := range s.Steps {
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n",
			stepTitle(st.Name), stepBadge(st.Status), escapeCell(Sanitize(st.Detail)), st.Duration.Round(100*time.Millisecond))
	}

	network := "已禁用"
	if s.Network != "none" {
		network = s.Network
	}
	kind := ""
	if label, ok := kindLabels[s.Kind]; ok {
		kind = "类型 " + label + " · "
	}
	fmt.Fprintf(b, "\n<sub>%s镜像 `%s` · 内存 %s · CPU %s · 网络 %s · 总耗时 %s</sub>\n",
		kind, escapeCell(s.Image), s.Memory, s.CPUs, network, s.Elapsed.Round(100*time.Millisecond))

	// 只展开没通过的步骤的输出，失败原因通常在末尾
	for _, st := range s.Steps {
		if st.Status == StepPassed || strings.TrimSpace(st.Output) == "" {
			continue
		}
		out := ansiRe.ReplaceAllString(st.Output, "")
		cut := st.Truncated
		if len(out) > maxStepOutput {
			out = out[len(out)-maxStepOutput:]
			cut = true
		}
		out = Sanitize(strings.ToValidUTF8(strings.TrimSpace(out), ""))
		out = textutil.TruncateUTF8(out, maxStepOutput+64)
		fence := fenceFor(out)
		note := "输出"
		if cut {
			note = "输出（仅末尾部分）"
		}
		fmt.Fprintf(b, "\n<details><summary>%s %s</summary>\n\n%s\n%s\n%s\n\n</details>\n", stepTitle(st.Name), note, fence, out, fence)
	}
}

// escapeCell 让文字可以安全放进 Markdown 表格单元格。
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r", ""), "\n", " ")
}
