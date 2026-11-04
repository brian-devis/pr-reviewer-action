// Package textutil 提供与文本处理相关的小工具。
package textutil

import "unicode/utf8"

// TruncateUTF8 在不破坏 UTF-8 字符的前提下，把 s 截断到最多 max 字节。
func TruncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}
