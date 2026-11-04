package review

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// MaxComments 是单个 PR 最多处理的评论数。prompt 里虽然也限制了条数，
// 但不能依赖模型遵守，这里是硬上限。
const MaxComments = 30

// similarThreshold 是判定两条评论“说的是同一件事”的相似度下限。
const similarThreshold = 0.15

var (
	// 只匹配真正会触发通知的提及：@ 前面不能是字母数字（邮箱里的 @ 不算）
	mentionRe = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9_-]+)`)
	headerRe  = regexp.MustCompile(`^\S+ \*\*\w+\*\*：`)
)

const zeroWidth = "​"

// Sanitize 处理模型生成的、会作为 Markdown 展示的文字：
//   - 去掉隐藏标记，避免伪造标记劫持总评更新和去重；
//   - 在 @ 后插入零宽字符，避免模型（或被注入的 diff）通过 @user 通知真实用户；
//   - 打断 HTML 注释开头，避免夹带不可见内容。
//
// 行内代码（成对反引号之间）不受影响，因为那里不会触发提及。
// 反引号数量为奇数时无法确定哪里是代码，整段按普通文字处理。
func Sanitize(s string) string {
	s = strings.ReplaceAll(s, Marker, "")
	parts := strings.Split(s, "`")
	step := 2
	if len(parts)%2 == 0 { // 反引号个数为奇数
		step = 1
	}
	for i := 0; i < len(parts); i += step {
		p := strings.ReplaceAll(parts[i], "<!--", "<!"+zeroWidth+"--")
		parts[i] = mentionRe.ReplaceAllString(p, "${1}@"+zeroWidth+"${2}")
	}
	return strings.Join(parts, "`")
}

// fenceFor 返回比 s 中任何连续反引号都长的围栏，保证代码块不会被内容提前闭合。
func fenceFor(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	n := 3
	if longest+1 > n {
		n = longest + 1
	}
	return strings.Repeat("`", n)
}

// Cap 在评论数超过 n 时保留严重度最高的 n 条，返回被丢弃的数量。
func Cap(comments []Comment, n int) (kept []Comment, dropped int) {
	if len(comments) <= n {
		return comments, 0
	}
	rank := map[string]int{"error": 0, "warning": 1, "info": 2}
	sorted := append([]Comment(nil), comments...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return rank[normSeverity(sorted[i].Severity)] < rank[normSeverity(sorted[j].Severity)]
	})
	return sorted[:n], len(comments) - n
}

// Similar 判断两条评论是否在说同一件事。模型每次运行措辞都会变，
// 所以不比较原文，而是比较去掉标记和“🟡 **warning**：”抬头之后的字符二元组重合度。
func Similar(a, b string) bool {
	return similarity(a, b) >= similarThreshold
}

// similarity 返回两段文字字符二元组集合的 Jaccard 系数，范围 0 到 1。
func similarity(a, b string) float64 {
	ga, gb := bigrams(commentText(a)), bigrams(commentText(b))
	if len(ga) == 0 || len(gb) == 0 {
		return 0
	}
	inter := 0
	for g := range ga {
		if gb[g] {
			inter++
		}
	}
	return float64(inter) / float64(len(ga)+len(gb)-inter)
}

func commentText(s string) string {
	return headerRe.ReplaceAllString(strings.TrimSpace(strings.ReplaceAll(s, Marker, "")), "")
}

func bigrams(s string) map[[2]rune]bool {
	var rs []rune
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			rs = append(rs, r)
		}
	}
	out := map[[2]rune]bool{}
	for i := 0; i+1 < len(rs); i++ {
		out[[2]rune{rs[i], rs[i+1]}] = true
	}
	return out
}
