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
			if desc == "" || len([]rune(desc)) > 140 {
				t.Fatalf("描述为空或超过 GitHub 的 140 字符上限: %q", desc)
			}
			for _, r := range desc {
				if r > 0xFFFF {
					t.Fatalf("描述含 4 字节 Unicode，GitHub 会返回 422: %q", desc)
				}
			}
		})
	}
}

func TestDecideDescriptionShowsCounts(t *testing.T) {
	_, desc := Decide(Counts{Error: 2, Warning: 1}, "error", false)
	if !strings.Contains(desc, "error 2") || !strings.Contains(desc, "warning 1") {
		t.Fatalf("描述应带统计: %q", desc)
	}
}

func TestValidSandboxFailOn(t *testing.T) {
	for _, ok := range []string{"never", "failure", "error", "any"} {
		if !ValidSandboxFailOn(ok) {
			t.Errorf("%q 应合法", ok)
		}
	}
	// warning 是 AI fail_on 的取值，不是沙盒的；大小写不合法由配置层统一小写处理
	for _, bad := range []string{"", "warning", "Failure", "all"} {
		if ValidSandboxFailOn(bad) {
			t.Errorf("%q 应不合法", bad)
		}
	}
}

func TestSandboxShouldFail(t *testing.T) {
	tests := []struct {
		failOn string
		state  string
		want   bool
	}{
		{"never", StateSuccess, false},
		{"never", StateFailure, false},
		{"never", StateError, false},
		{"failure", StateSuccess, false},
		{"failure", StateFailure, true},
		{"failure", StateError, false},
		{"error", StateSuccess, false},
		{"error", StateFailure, false},
		{"error", StateError, true},
		{"any", StateSuccess, false},
		{"any", StateFailure, true},
		{"any", StateError, true},
		// 未知取值按 never 处理，不应该意外让步骤失败
		{"", StateFailure, false},
		{"bogus", StateError, false},
	}
	for _, tt := range tests {
		if got := SandboxShouldFail(tt.state, tt.failOn); got != tt.want {
			t.Errorf("SandboxShouldFail(%q, %q) = %v, want %v", tt.state, tt.failOn, got, tt.want)
		}
	}
}
