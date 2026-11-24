package review

import (
	"strings"
	"testing"
)

// TestSystemPromptKeepsKnownFalsePositiveRules 锁定 system prompt 里针对已知误报的规则。
// 这些规则是模型行为约束，只能在真实模型上验证效果（见 live_test.go），
// 但至少要在每次修改 prompt 时确认它们没有被误删或改写。
func TestSystemPromptKeepsKnownFalsePositiveRules(t *testing.T) {
	sys, _ := Prompt(nil)

	rules := []struct {
		name    string
		phrases []string
	}{
		{
			// 反馈 1：同级的 if/runs-on 被误报为缩进错误，并触发 fail_on: error
			name:    "格式与缩进不得仅凭局部 diff 推测，且不得定为 error",
			phrases: []string{"不得仅凭局部 diff 推测", "不得标为 error", "已经对齐", "相同的修复建议"},
		},
		{
			// fail_on: error 的有效性依赖 error 有确凿证据
			name:    "error 必须具备证据",
			phrases: []string{"证据要求", "有问题的代码、触发条件和实际后果", "证据不足就降为"},
		},
		{
			// 反馈 2：模型建议改用 pull_request_target，与 README 的安全要求冲突
			name:    "fork PR 安全硬规则",
			phrases: []string{"禁止建议用 pull_request_target", "禁止在持有 secrets 的工作流中检出或执行不可信 fork 代码", "pull_request 事件与最小权限"},
		},
		{
			// 反馈 3：enable_ai_review=false 时质疑缺少 api_key
			name:    "注入本 Action 的输入语义",
			phrases: []string{"两个独立开关", "enable_ai_review 为 false 时不要求 api_key", "ErrNoCommands", "不能因为配置里没写就认为缺失"},
		},
	}
	for _, r := range rules {
		for _, p := range r.phrases {
			if !strings.Contains(sys, p) {
				t.Errorf("%s：prompt 中缺少 %q", r.name, p)
			}
		}
	}
}

// TestSystemPromptKeepsOutputContract 确认增强规则没有改动输出格式约定，
// 否则解析层和 API 适配器都会受影响。
func TestSystemPromptKeepsOutputContract(t *testing.T) {
	sys, _ := Prompt(nil)
	for _, want := range []string{
		`{"summary":"...","comments":[{"file":"路径","line":12,"severity":"error|warning|info","message":"...","suggestion":"..."}]}`,
		`"### FILE:"`,
		"L数字",
		"最多 15 条",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("prompt 中缺少输出约定 %q", want)
		}
	}
}
