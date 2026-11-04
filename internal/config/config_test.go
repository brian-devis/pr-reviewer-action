package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func setupEnv(t *testing.T, event string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "event.json")
	if err := os.WriteFile(path, []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_EVENT_PATH", path)
	t.Setenv("GITHUB_REPOSITORY", "o/r")
	t.Setenv("INPUT_GITHUB_TOKEN", "ghs_x")
	t.Setenv("INPUT_API_KEY", "sk-x")
	t.Setenv("INPUT_API_BASE_URL", "https://example.com/v1/")
	t.Setenv("INPUT_MODEL", "")
	t.Setenv("INPUT_API_FORMAT", "")
	t.Setenv("INPUT_MAX_DIFF_BYTES", "")
	t.Setenv("INPUT_MAX_REQUESTS", "")
	t.Setenv("INPUT_EXCLUDE_PATHS", "docs/, *.md\n  gen/*.go ,")
	for _, k := range []string{
		"INPUT_FAIL_ON", "INPUT_STATUS_CONTEXT",
		"INPUT_ENABLE_AI_REVIEW", "INPUT_ENABLE_SANDBOX", "INPUT_SANDBOX_IMAGE",
		"INPUT_SANDBOX_TEST_COMMAND", "INPUT_SANDBOX_LINT_COMMAND", "INPUT_SANDBOX_MEMORY",
		"INPUT_SANDBOX_CPUS", "INPUT_SANDBOX_TIMEOUT", "INPUT_SANDBOX_NETWORK",
		"INPUT_SANDBOX_STATUS_CONTEXT", "GITHUB_WORKSPACE", "GITHUB_SERVER_URL", "GITHUB_RUN_ID",
	} {
		t.Setenv(k, "")
	}
}

func TestLoad(t *testing.T) {
	setupEnv(t, `{"pull_request":{"number":7,"head":{"sha":"abc123"}}}`)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.PRNumber != 7 || c.HeadSHA != "abc123" {
		t.Fatalf("PR=%d sha=%s", c.PRNumber, c.HeadSHA)
	}
	if c.APIBaseURL != "https://example.com/v1" {
		t.Fatalf("base url = %q", c.APIBaseURL)
	}
	if c.MaxDiffBytes != 60000 || c.MaxRequests != 5 || c.Model != "gpt-4.1" {
		t.Fatalf("defaults wrong: %+v", c)
	}
	want := []string{"docs/", "*.md", "gen/*.go"}
	if !reflect.DeepEqual(c.Exclude, want) {
		t.Fatalf("exclude = %v, want %v", c.Exclude, want)
	}
}

func TestLoadRejectsNonPREvent(t *testing.T) {
	setupEnv(t, `{"ref":"refs/heads/main"}`)
	if _, err := Load(); err == nil {
		t.Fatal("expected error for non-PR event")
	}
}

func TestLoadRejectsUnknownFormat(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_API_FORMAT", "gemini")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for unsupported api_format")
	}
}

func TestLoadFormatDefaults(t *testing.T) {
	tests := []struct {
		format, baseURL, model string
	}{
		{"openai", "https://api.openai.com/v1", "gpt-4.1"},
		{"anthropic", "https://api.anthropic.com", "claude-sonnet-5-5"},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			setupEnv(t, `{"number":1}`)
			t.Setenv("INPUT_API_FORMAT", tt.format)
			t.Setenv("INPUT_API_BASE_URL", "")
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.APIBaseURL != tt.baseURL || c.Model != tt.model {
				t.Fatalf("base=%q model=%q", c.APIBaseURL, c.Model)
			}
		})
	}
}

func TestLoadFormatIsCaseInsensitive(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_API_FORMAT", " Anthropic ")
	c, err := Load()
	if err != nil || c.APIFormat != "anthropic" {
		t.Fatalf("format=%q err=%v", c.APIFormat, err)
	}
}

func TestLoadExplicitValuesOverrideDefaults(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_API_FORMAT", "anthropic")
	t.Setenv("INPUT_MODEL", "my-model")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIBaseURL != "https://example.com/v1" || c.Model != "my-model" {
		t.Fatalf("base=%q model=%q", c.APIBaseURL, c.Model)
	}
}

func TestLoadFailOn(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	c, err := Load()
	if err != nil || c.FailOn != "never" || c.StatusContext != "ai-review" {
		t.Fatalf("默认值错误: %+v err=%v", c, err)
	}

	t.Setenv("INPUT_FAIL_ON", " Warning ")
	t.Setenv("INPUT_STATUS_CONTEXT", "my-check")
	c, err = Load()
	if err != nil || c.FailOn != "warning" || c.StatusContext != "my-check" {
		t.Fatalf("自定义值错误: %+v err=%v", c, err)
	}

	t.Setenv("INPUT_FAIL_ON", "any")
	if _, err := Load(); err == nil {
		t.Fatal("无效的 fail_on 应报错")
	}
}

// sandboxOnly 准备“只开沙盒、没有任何模型配置”的环境。
func sandboxOnly(t *testing.T) {
	t.Helper()
	setupEnv(t, `{"pull_request":{"number":3,"head":{"sha":"headsha"}}}`)
	t.Setenv("INPUT_ENABLE_AI_REVIEW", "false")
	t.Setenv("INPUT_ENABLE_SANDBOX", "true")
	t.Setenv("INPUT_API_KEY", "")
	t.Setenv("INPUT_API_BASE_URL", "")
	t.Setenv("INPUT_API_FORMAT", "")
	t.Setenv("INPUT_MODEL", "")
	t.Setenv("GITHUB_WORKSPACE", t.TempDir())
}

func TestSandboxOnlyNeedsNoModelConfig(t *testing.T) {
	sandboxOnly(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.EnableAI || !c.EnableSandbox {
		t.Fatalf("EnableAI=%v EnableSandbox=%v", c.EnableAI, c.EnableSandbox)
	}
	if c.APIKey != "" || c.APIBaseURL != "" || c.Model != "" {
		t.Fatalf("关闭 AI 时不应读取模型配置: %+v", c)
	}
	if c.HeadSHA != "headsha" {
		t.Fatalf("应使用 PR 的 head sha，got %q", c.HeadSHA)
	}
}

func TestSandboxOnlyIgnoresInvalidAIValues(t *testing.T) {
	sandboxOnly(t)
	t.Setenv("INPUT_API_FORMAT", "not-a-format")
	t.Setenv("INPUT_FAIL_ON", "bogus")
	t.Setenv("INPUT_MAX_DIFF_BYTES", "abc")
	if _, err := Load(); err != nil {
		t.Fatalf("关闭 AI 后，AI 相关的无效值不应导致失败: %v", err)
	}
}

func TestBothDisabledIsAllowed(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_ENABLE_AI_REVIEW", "false")
	t.Setenv("INPUT_API_KEY", "")
	c, err := Load()
	if err != nil || c.EnableAI || c.EnableSandbox {
		t.Fatalf("两个都关闭应正常加载: %+v err=%v", c, err)
	}
}

func TestAIEnabledStillRequiresKey(t *testing.T) {
	setupEnv(t, `{"number":1}`)
	t.Setenv("INPUT_API_KEY", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "enable_ai_review") {
		t.Fatalf("应提示可以关闭 AI 评审: %v", err)
	}
}

func TestEnableFlagsParsing(t *testing.T) {
	for _, tt := range []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"true", true, false}, {" TRUE ", true, false}, {"1", true, false},
		{"false", false, false}, {"False", false, false}, {"0", false, false},
		{"maybe", false, true},
	} {
		setupEnv(t, `{"number":1}`)
		t.Setenv("INPUT_ENABLE_AI_REVIEW", "false")
		t.Setenv("INPUT_ENABLE_SANDBOX", tt.val)
		t.Setenv("GITHUB_WORKSPACE", t.TempDir())
		c, err := Load()
		if (err != nil) != tt.wantErr {
			t.Errorf("%q: err=%v", tt.val, err)
			continue
		}
		if err == nil && c.EnableSandbox != tt.want {
			t.Errorf("%q: got %v, want %v", tt.val, c.EnableSandbox, tt.want)
		}
	}
}

func TestSandboxDefaults(t *testing.T) {
	sandboxOnly(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxMemory != "1g" || c.SandboxCPUs != "1" || c.SandboxTimeout != 5*time.Minute ||
		c.SandboxStatusContext != "sandbox-tests" {
		t.Fatalf("默认值错误: %+v", c)
	}
}

func TestSandboxCustomValues(t *testing.T) {
	sandboxOnly(t)
	t.Setenv("INPUT_SANDBOX_IMAGE", "golang:1.25")
	t.Setenv("INPUT_SANDBOX_TEST_COMMAND", " go test ./... ")
	t.Setenv("INPUT_SANDBOX_LINT_COMMAND", "go vet ./...")
	t.Setenv("INPUT_SANDBOX_MEMORY", "512M")
	t.Setenv("INPUT_SANDBOX_CPUS", "0.5")
	t.Setenv("INPUT_SANDBOX_TIMEOUT", "90s")
	t.Setenv("INPUT_SANDBOX_STATUS_CONTEXT", "my-sandbox")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxImage != "golang:1.25" || c.SandboxTestCmd != "go test ./..." || c.SandboxLintCmd != "go vet ./..." ||
		c.SandboxMemory != "512m" || c.SandboxCPUs != "0.5" || c.SandboxTimeout != 90*time.Second ||
		c.SandboxStatusContext != "my-sandbox" {
		t.Fatalf("%+v", c)
	}
}

func TestSandboxRejectsUnsafeOrInvalidValues(t *testing.T) {
	tests := []struct{ name, key, val string }{
		{"网络不能开放", "INPUT_SANDBOX_NETWORK", "bridge"},
		{"网络不能是 host", "INPUT_SANDBOX_NETWORK", "host"},
		{"镜像名不能是选项", "INPUT_SANDBOX_IMAGE", "--privileged"},
		{"镜像名不能带空格", "INPUT_SANDBOX_IMAGE", "alpine -v /:/h"},
		{"内存格式错误", "INPUT_SANDBOX_MEMORY", "lots"},
		{"内存不能为零", "INPUT_SANDBOX_MEMORY", "0"},
		{"cpu 不能为零", "INPUT_SANDBOX_CPUS", "0"},
		{"cpu 过大", "INPUT_SANDBOX_CPUS", "1000"},
		{"cpu 不是数字", "INPUT_SANDBOX_CPUS", "x"},
		{"超时格式错误", "INPUT_SANDBOX_TIMEOUT", "soon"},
		{"超时不能为零", "INPUT_SANDBOX_TIMEOUT", "0s"},
		{"超时过长", "INPUT_SANDBOX_TIMEOUT", "5h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxOnly(t)
			t.Setenv(tt.key, tt.val)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q 应被拒绝", tt.key, tt.val)
			}
		})
	}
}

func TestSandboxNetworkNoneAccepted(t *testing.T) {
	sandboxOnly(t)
	t.Setenv("INPUT_SANDBOX_NETWORK", "NONE")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxRequiresWorkspace(t *testing.T) {
	sandboxOnly(t)
	t.Setenv("GITHUB_WORKSPACE", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("应提示先 checkout: %v", err)
	}
}

func TestRunURL(t *testing.T) {
	sandboxOnly(t)
	t.Setenv("GITHUB_SERVER_URL", "https://github.com/")
	t.Setenv("GITHUB_RUN_ID", "42")
	c, err := Load()
	if err != nil || c.RunURL != "https://github.com/o/r/actions/runs/42" {
		t.Fatalf("RunURL=%q err=%v", c.RunURL, err)
	}
}
