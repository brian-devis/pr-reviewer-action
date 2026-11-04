package review

import (
	"strings"
	"testing"
)

func TestCount(t *testing.T) {
	a := []Comment{{Severity: "error"}, {Severity: "WARNING"}}
	b := []Comment{{Severity: "info"}, {Severity: "weird"}} // 未知严重度按 warning 处理
	got := Count(a, b)
	if got != (Counts{Error: 1, Warning: 2, Info: 1}) {
		t.Fatalf("got %+v", got)
	}
	if Count() != (Counts{}) {
		t.Fatal("空输入应全为 0")
	}
}

func TestValidFailOn(t *testing.T) {
	for _, ok := range []string{"never", "error", "warning"} {
		if !ValidFailOn(ok) {
			t.Errorf("%q 应合法", ok)
		}
	}
	for _, bad := range []string{"", "any", "Error", "info"} {
		if ValidFailOn(bad) {
			t.Errorf("%q 应不合法", bad)
		}
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		c          Counts
		failOn     string
		incomplete bool
		want       string
	}{
		{"never 即使有 error 也通过", Counts{Error: 3}, "never", false, StateSuccess},
		{"error 阈值命中", Counts{Error: 1}, "error", false, StateFailure},
		{"error 阈值下只有 warning", Counts{Warning: 5}, "error", false, StateSuccess},
		{"warning 阈值命中 warning", Counts{Warning: 1}, "warning", false, StateFailure},
		{"warning 阈值下只有 info", Counts{Info: 4}, "warning", false, StateSuccess},
		{"没有问题", Counts{}, "error", false, StateSuccess},
		{"启用阈值但不完整且没超标", Counts{Info: 1}, "error", true, StateError},
		{"never 不完整仍是通过", Counts{}, "never", true, StateSuccess},
		{"超标优先于不完整", Counts{Error: 1}, "error", true, StateFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, desc := Decide(tt.c, tt.failOn, tt.incomplete)
			if state != tt.want {
				t.Fatalf("state=%s, want %s (%s)", state, tt.want, desc)
			}
			if desc == "" || len(desc) > 140 {
				t.Fatalf("描述为空或超过 GitHub 的 140 字符上限: %q", desc)
			}
		})
	}
}

func TestDecideDescriptionShowsCounts(t *testing.T) {
	_, desc := Decide(Counts{Error: 2, Warning: 1}, "error", false)
	if !strings.Contains(desc, "🔴 2") || !strings.Contains(desc, "🟡 1") {
		t.Fatalf("描述应带统计: %q", desc)
	}
}
