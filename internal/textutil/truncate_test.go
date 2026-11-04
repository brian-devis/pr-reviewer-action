package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"短于上限", "abc", 10, "abc"},
		{"ASCII 截断", "abcdef", 3, "abc"},
		{"不切断汉字", "你好世界", 4, "你"},
		{"恰好整字", "你好世界", 6, "你好"},
		{"零上限", "abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateUTF8(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("结果不是合法 UTF-8: %q", got)
			}
		})
	}
}
