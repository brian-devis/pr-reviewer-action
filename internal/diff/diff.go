// Package diff 解析 unified diff，给每个文件生成带行号标注的文本，
// 并记录哪些新文件行可以挂行内评论。
package diff

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/brian-devis/pr-reviewer-action/internal/textutil"
)

// File 是 diff 中的一个文件。
type File struct {
	Path      string
	Binary    bool
	Deleted   bool
	Truncated bool
	// Lines 是新文件中出现在 diff 里的行号（新增行和上下文行），
	// GitHub 只允许在这些行上挂行内评论。
	Lines map[int]bool
	// Text 是发给模型的标注文本：新增行/上下文行带 "L<行号>" 前缀。
	Text string
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Parse 解析 unified diff。
func Parse(raw string) []*File {
	var (
		files  []*File
		cur    *File
		body   strings.Builder
		newLn  int
		inHunk bool
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = "### FILE: " + cur.Path + "\n" + body.String()
		files = append(files, cur)
		body.Reset()
	}

	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &File{Lines: map[int]bool{}}
			inHunk = false
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				cur.Path = line[i+3:]
			}
		case cur == nil:
			continue
		case !inHunk && strings.HasPrefix(line, "+++ "):
			if p := line[4:]; p == "/dev/null" {
				cur.Deleted = true
			} else {
				cur.Path = strings.TrimPrefix(p, "b/")
			}
		case !inHunk && (strings.HasPrefix(line, "Binary files") || strings.HasPrefix(line, "GIT binary patch")):
			cur.Binary = true
		case strings.HasPrefix(line, "@@"):
			m := hunkRe.FindStringSubmatch(line)
			if m == nil {
				inHunk = false
				continue
			}
			newLn, _ = strconv.Atoi(m[1])
			inHunk = true
			body.WriteString(line + "\n")
		case inHunk && line != "":
			switch line[0] {
			case '+':
				cur.Lines[newLn] = true
				fmt.Fprintf(&body, "L%d + %s\n", newLn, line[1:])
				newLn++
			case ' ':
				cur.Lines[newLn] = true
				fmt.Fprintf(&body, "L%d   %s\n", newLn, line[1:])
				newLn++
			case '-':
				fmt.Fprintf(&body, "     - %s\n", line[1:])
			}
		}
	}
	flush()
	return files
}

var defaultExcludes = []string{
	"package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.sum", "Cargo.lock",
	"poetry.lock", "composer.lock", "Gemfile.lock",
	"vendor/", "node_modules/", "dist/",
	"*.min.js", "*.min.css", "*.pb.go", "*_generated.go", "*.snap",
}

// Excluded 判断路径是否匹配任一模式。以 "/" 结尾的模式匹配目录，
// 其余模式按 path.Match 匹配完整路径或文件名。
func Excluded(p string, patterns []string) bool {
	for _, pat := range patterns {
		if strings.HasSuffix(pat, "/") {
			if strings.HasPrefix(p, pat) || strings.Contains(p, "/"+pat) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
		if ok, _ := path.Match(pat, path.Base(p)); ok {
			return true
		}
	}
	return false
}

// Select 去掉二进制、被删除、没有可评论行以及被排除的文件。
func Select(files []*File, userExcludes []string) []*File {
	var out []*File
	for _, f := range files {
		if f.Binary || f.Deleted || len(f.Lines) == 0 {
			continue
		}
		if Excluded(f.Path, defaultExcludes) || Excluded(f.Path, userExcludes) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Chunk 按字节预算把文件顺序分组，每组对应一次模型请求。
// 单个文件超过预算时会被截断；超出 maxChunks 的文件放进 skipped。
func Chunk(files []*File, budget, maxChunks int) (chunks [][]*File, skipped []*File) {
	var cur []*File
	size := 0
	for _, f := range files {
		if len(f.Text) > budget {
			f.Text = truncateLines(f.Text, budget)
			f.Truncated = true
		}
		if len(cur) > 0 && size+len(f.Text) > budget {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		if len(chunks) >= maxChunks {
			skipped = append(skipped, f)
			continue
		}
		cur = append(cur, f)
		size += len(f.Text)
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks, skipped
}

func truncateLines(s string, budget int) string {
	s = textutil.TruncateUTF8(s, budget)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[:i]
	}
	return s + "\n[... 文件过大，后续内容已截断]\n"
}
