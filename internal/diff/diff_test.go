package diff

import (
	"strings"
	"testing"
)

const sample = `diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -1,3 +1,4 @@
 package a
-var x = 1
+var x = 2
+var y = 3
 func f() {}
diff --git a/old.txt b/old.txt
deleted file mode 100644
index 3333333..0000000
--- a/old.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/img.png b/img.png
index 4444444..5555555 100644
Binary files a/img.png and b/img.png differ
diff --git a/pkg/b.go b/pkg/b.go
--- a/pkg/b.go
+++ b/pkg/b.go
@@ -10,2 +10,3 @@ func g() {
 keep
+added
 tail
`

func TestParse(t *testing.T) {
	files := Parse(sample)
	if len(files) != 4 {
		t.Fatalf("got %d files, want 4", len(files))
	}

	a := files[0]
	if a.Path != "a.go" {
		t.Fatalf("path = %q", a.Path)
	}
	for _, ln := range []int{1, 2, 3, 4} {
		if !a.Lines[ln] {
			t.Errorf("a.go 行 %d 应该可评论", ln)
		}
	}
	if len(a.Lines) != 4 {
		t.Errorf("a.go 可评论行数 = %d, want 4", len(a.Lines))
	}
	for _, want := range []string{"L2 + var x = 2", "L3 + var y = 3", "     - var x = 1", "L4   func f() {}"} {
		if !strings.Contains(a.Text, want) {
			t.Errorf("a.go 标注文本缺少 %q:\n%s", want, a.Text)
		}
	}

	if !files[1].Deleted || len(files[1].Lines) != 0 {
		t.Errorf("old.txt 应为已删除且无可评论行: %+v", files[1])
	}
	if !files[2].Binary {
		t.Errorf("img.png 应为二进制")
	}
	b := files[3]
	if b.Path != "pkg/b.go" || !b.Lines[10] || !b.Lines[11] || !b.Lines[12] {
		t.Errorf("pkg/b.go 解析错误: path=%q lines=%v", b.Path, b.Lines)
	}
}

func TestSelect(t *testing.T) {
	raw := sample + `diff --git a/go.sum b/go.sum
--- a/go.sum
+++ b/go.sum
@@ -1 +1,2 @@
 x
+y
diff --git a/docs/readme.md b/docs/readme.md
--- a/docs/readme.md
+++ b/docs/readme.md
@@ -1 +1,2 @@
 x
+y
`
	got := Select(Parse(raw), []string{"docs/"})
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, ",") != "a.go,pkg/b.go" {
		t.Fatalf("selected = %v", paths)
	}
}

func TestExcluded(t *testing.T) {
	tests := []struct {
		path string
		pats []string
		want bool
	}{
		{"web/package-lock.json", []string{"package-lock.json"}, true},
		{"vendor/x/y.go", []string{"vendor/"}, true},
		{"a/vendor/y.go", []string{"vendor/"}, true},
		{"avendor/y.go", []string{"vendor/"}, false},
		{"app.min.js", []string{"*.min.js"}, true},
		{"gen/x.go", []string{"gen/*.go"}, true},
		{"gen/sub/x.go", []string{"gen/*.go"}, false}, // * 不跨目录
	}
	for _, tt := range tests {
		got := Excluded(tt.path, tt.pats)
		if got != tt.want {
			t.Errorf("Excluded(%q, %v) = %v, want %v", tt.path, tt.pats, got, tt.want)
		}
	}
}

func mk(name string, size int) *File {
	return &File{Path: name, Lines: map[int]bool{1: true}, Text: strings.Repeat("x\n", size/2)}
}

func TestChunk(t *testing.T) {
	files := []*File{mk("a", 40), mk("b", 40), mk("c", 40), mk("d", 40)}
	chunks, skipped := Chunk(files, 100, 5)
	if len(chunks) != 2 || len(chunks[0]) != 2 || len(chunks[1]) != 2 || len(skipped) != 0 {
		t.Fatalf("chunks=%d skipped=%d", len(chunks), len(skipped))
	}

	files = []*File{mk("a", 40), mk("b", 40), mk("c", 40), mk("d", 40)}
	chunks, skipped = Chunk(files, 100, 1)
	if len(chunks) != 1 || len(chunks[0]) != 2 || len(skipped) != 2 {
		t.Fatalf("限制批数后 chunks=%d skipped=%d", len(chunks), len(skipped))
	}
}

func TestChunkTruncatesOversizedFile(t *testing.T) {
	f := mk("big", 1000)
	chunks, _ := Chunk([]*File{f}, 100, 3)
	if len(chunks) != 1 || !f.Truncated {
		t.Fatalf("超大文件应被截断: truncated=%v", f.Truncated)
	}
	if !strings.Contains(f.Text, "已截断") || len(f.Text) > 150 {
		t.Fatalf("截断后文本异常 (len=%d)", len(f.Text))
	}
}
