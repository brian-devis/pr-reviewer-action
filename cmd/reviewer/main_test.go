package main

import (
	"testing"

	"github.com/brian-devis/pr-reviewer-action/internal/ghclient"
	"github.com/brian-devis/pr-reviewer-action/internal/review"
)

func TestFilterFresh(t *testing.T) {
	botBody := "旧措辞\n\n" + review.Marker
	existing := []ghclient.ExistingComment{
		{Path: "a.go", Line: 3, Body: botBody},
		{Path: "a.go", Line: 5, Body: "人工评论，没有标记"},
		{Path: "b.go", Line: 0, Body: botBody}, // 已过期的评论，行号为 0
	}
	inline := []review.Comment{
		{File: "a.go", Line: 3, Severity: "warning", Message: "新措辞，但同一位置"},
		{File: "a.go", Line: 5, Severity: "info", Message: "人工评论所在行仍可发"},
		{File: "a.go", Line: 8, Severity: "error", Message: "全新位置"},
		{File: "a.go", Line: 8, Severity: "info", Message: "同一位置第二条"},
	}

	got := filterFresh(inline, existing)
	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2: %+v", len(got), got)
	}
	if got[0].Line != 5 || got[1].Line != 8 {
		t.Fatalf("应保留第 5、8 行，实际 %d、%d", got[0].Line, got[1].Line)
	}
	if got[0].Side != "RIGHT" || got[0].Path != "a.go" {
		t.Fatalf("字段错误: %+v", got[0])
	}
}
