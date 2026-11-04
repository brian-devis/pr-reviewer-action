package main

import (
	"testing"

	"github.com/brian-devis/pr-reviewer-action/internal/ghclient"
	"github.com/brian-devis/pr-reviewer-action/internal/review"
)

func bot(path string, line int, c review.Comment) ghclient.ExistingComment {
	return ghclient.ExistingComment{Path: path, Line: line, Body: review.RenderInline(c)}
}

func TestFilterFresh(t *testing.T) {
	sqlOld := review.Comment{Severity: "warning", Message: "SQL 拼接了用户输入，存在注入风险，应使用参数化查询。"}
	sqlNew := review.Comment{Severity: "error", Message: "这里直接拼接 SQL，存在 SQL 注入风险，建议改用参数化查询。"}
	other := review.Comment{Severity: "warning", Message: "错误被忽略：err 赋值后没有检查，调用方会拿到空结果。"}

	existing := []ghclient.ExistingComment{
		bot("a.go", 3, sqlOld),
		{Path: "a.go", Line: 5, Body: "人工评论，没有标记"},
		{Path: "b.go", Line: 0, Body: review.RenderInline(sqlOld)}, // 已过期的评论，行号为 0
	}

	tests := []struct {
		name    string
		comment review.Comment
		line    int
		want    bool // 是否应该发出
	}{
		{"同一位置换了措辞", sqlNew, 3, false},
		{"同一位置的新问题", other, 3, true},
		{"人工评论所在行", sqlNew, 5, true},
		{"全新位置", sqlNew, 8, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.comment.File, tt.comment.Line = "a.go", review.Line(tt.line)
			got := filterFresh([]review.Comment{tt.comment}, existing)
			if (len(got) == 1) != tt.want {
				t.Fatalf("发出=%v，期望 %v", len(got) == 1, tt.want)
			}
		})
	}
}

func TestFilterFreshLimitsPerLocation(t *testing.T) {
	msgs := []string{
		"变量命名不清晰，建议改成更具描述性的名字",
		"函数过长，建议拆分成多个小函数提高可读性",
		"缺少单元测试覆盖该分支的边界条件",
		"并发访问共享 map 没有加锁会导致数据竞争",
	}
	var inline []review.Comment
	for _, m := range msgs {
		inline = append(inline, review.Comment{File: "a.go", Line: 1, Severity: "info", Message: m})
	}
	got := filterFresh(inline, nil)
	if len(got) != maxBotPerLocation {
		t.Fatalf("同一位置最多 %d 条，实际 %d", maxBotPerLocation, len(got))
	}
}
