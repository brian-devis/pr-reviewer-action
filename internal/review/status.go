package review

import "fmt"

// Commit status 的状态值，与 GitHub API 一致。
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// Counts 是各严重度的问题数量。
type Counts struct{ Error, Warning, Info int }

// Count 统计评论的严重度分布。
func Count(comments ...[]Comment) Counts {
	var c Counts
	for _, list := range comments {
		for _, cm := range list {
			switch normSeverity(cm.Severity) {
			case "error":
				c.Error++
			case "warning":
				c.Warning++
			default:
				c.Info++
			}
		}
	}
	return c
}

// ValidFailOn 报告 fail_on 的取值是否合法。
func ValidFailOn(s string) bool {
	return s == "never" || s == "error" || s == "warning"
}

// Decide 根据问题数量、fail_on 阈值和审查是否完整，给出 commit status 的状态和描述。
// incomplete 表示有模型请求失败或有文件因额度被跳过。
//
// fail_on=never 时只做提示，不会阻断合并，除非审查完全没有产出（error 由调用方单独处理）。
// 启用了阈值但审查不完整时返回 error：没审完就不能声称“通过”。
func Decide(c Counts, failOn string, incomplete bool) (state, desc string) {
	summary := fmt.Sprintf("🔴 %d · 🟡 %d · 🔵 %d", c.Error, c.Warning, c.Info)

	exceeded := (failOn == "error" && c.Error > 0) || (failOn == "warning" && c.Error+c.Warning > 0)
	switch {
	case exceeded:
		return StateFailure, "发现达到阈值的问题：" + summary
	case incomplete && failOn != "never":
		return StateError, "审查不完整，部分文件未被审查：" + summary
	case incomplete:
		return StateSuccess, "审查完成（部分文件未审查）：" + summary
	case c.Error+c.Warning+c.Info == 0:
		return StateSuccess, "未发现明显问题"
	default:
		return StateSuccess, "审查完成：" + summary
	}
}
