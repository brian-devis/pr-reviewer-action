package main_test

import "testing"

// 故意失败，用来验证沙盒在真实 PR 上会把状态设为 failure。验证完会删除。
func TestIntentionalFailure(t *testing.T) { t.Fatal("intentional failure for sandbox verification") }
