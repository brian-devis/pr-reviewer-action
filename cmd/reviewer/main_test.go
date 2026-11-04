package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/config"
	"github.com/brian-devis/pr-reviewer-action/internal/diff"
	"github.com/brian-devis/pr-reviewer-action/internal/ghclient"
	"github.com/brian-devis/pr-reviewer-action/internal/llm"
	"github.com/brian-devis/pr-reviewer-action/internal/review"
	"github.com/brian-devis/pr-reviewer-action/internal/sandbox"
)

// fakeProvider 按顺序返回预设的回复。
type fakeProvider struct {
	replies []string
	err     error
	calls   int
}

func (f *fakeProvider) Complete(context.Context, llm.Request) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	r := f.replies[f.calls]
	f.calls++
	return r, nil
}

const goodReply = `{"summary":"ok","comments":[{"file":"a.go","line":1,"severity":"info","message":"m"}]}`

func TestReviewChunkRetriesOnBadJSON(t *testing.T) {
	// 第一次是字符串里带未转义引号的坏 JSON，第二次正常
	p := &fakeProvider{replies: []string{`{"summary":"他说"不行"","comments":[]}`, goodReply}}
	res, _, ok, err := reviewChunk(context.Background(), p, "m", []*diff.File{{Text: "x"}})
	if err != nil || !ok || p.calls != 2 || len(res.Comments) != 1 {
		t.Fatalf("ok=%v err=%v calls=%d res=%+v", ok, err, p.calls, res)
	}
}

func TestReviewChunkGivesUpAfterMaxAttempts(t *testing.T) {
	p := &fakeProvider{replies: []string{"不是 JSON", "还是不是"}}
	_, text, ok, err := reviewChunk(context.Background(), p, "m", nil)
	if err != nil || ok || p.calls != maxParseAttempts {
		t.Fatalf("ok=%v err=%v calls=%d", ok, err, p.calls)
	}
	if text != "还是不是" {
		t.Fatalf("应保留最后一次的原文，got %q", text)
	}
}

func TestReviewChunkDoesNotRetryRequestErrors(t *testing.T) {
	p := &fakeProvider{err: errors.New("401")}
	_, _, ok, err := reviewChunk(context.Background(), p, "m", nil)
	if err == nil || ok {
		t.Fatalf("请求错误应直接返回: ok=%v err=%v", ok, err)
	}
}

func TestNewProvider(t *testing.T) {
	if _, ok := newProvider(&config.Config{APIFormat: "anthropic"}, nil).(*llm.Anthropic); !ok {
		t.Error("anthropic 应使用 Anthropic 实现")
	}
	if _, ok := newProvider(&config.Config{APIFormat: "openai"}, nil).(*llm.OpenAI); !ok {
		t.Error("openai 应使用 OpenAI 实现")
	}
}

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

// fakeSandbox 记录运行过的容器，并按步骤名返回预设的退出码。
type fakeSandbox struct {
	exit        map[string]int
	oom         map[string]bool
	unavailable error
	specs       []sandbox.Spec
}

func (f *fakeSandbox) Available(context.Context) error { return f.unavailable }

func (f *fakeSandbox) Run(_ context.Context, s sandbox.Spec) sandbox.Result {
	f.specs = append(f.specs, s)
	name := strings.Split(s.Name, "-")[1] // prr-<step>-<随机>
	r := sandbox.Result{Name: name, ExitCode: f.exit[name], OOM: f.oom[name], Duration: time.Second}
	if r.ExitCode != 0 {
		r.Output = name + " failed output"
	}
	r.Status = sandbox.Classify(r)
	return r
}

func sandboxConfig(t *testing.T, goProject bool) *config.Config {
	dir := t.TempDir()
	if goProject {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &config.Config{
		EnableSandbox: true, Workspace: dir, SandboxMemory: "1g", SandboxCPUs: "1", SandboxTimeout: time.Minute,
	}
}

func TestRunSandboxPassed(t *testing.T) {
	f := &fakeSandbox{}
	sec, state, desc, err := runSandbox(context.Background(), sandboxConfig(t, true), f)
	if err != nil || state != "success" || desc != "All sandbox checks passed" {
		t.Fatalf("state=%s desc=%q err=%v", state, desc, err)
	}
	if sec.Network != "none" || sec.Image != "golang:1.22" || len(sec.Steps) != 2 {
		t.Fatalf("section 异常: %+v", sec)
	}
}

func TestRunSandboxTestFailureIsFailureNotError(t *testing.T) {
	f := &fakeSandbox{exit: map[string]int{"test": 1}}
	sec, state, desc, err := runSandbox(context.Background(), sandboxConfig(t, true), f)
	if err != nil {
		t.Fatalf("测试没通过不是沙盒错误: %v", err)
	}
	if state != "failure" || desc != "Sandbox checks failed" {
		t.Fatalf("state=%s desc=%q", state, desc)
	}
	if len(sec.Steps) != 2 || sec.Steps[0].Status != review.StepFailed || sec.Steps[1].Status != review.StepPassed {
		t.Fatalf("test 失败后 lint 仍应运行并通过: %+v", sec.Steps)
	}
	if !strings.Contains(sec.Steps[0].Output, "test failed output") {
		t.Fatalf("失败输出应被带入总评: %+v", sec.Steps[0])
	}
}

func TestRunSandboxOOMIsError(t *testing.T) {
	f := &fakeSandbox{exit: map[string]int{"lint": 137}, oom: map[string]bool{"lint": true}}
	_, state, desc, err := runSandbox(context.Background(), sandboxConfig(t, true), f)
	if err != nil || state != "error" || desc != "Sandbox execution error" {
		t.Fatalf("OOM 应为 error 状态: state=%s desc=%q err=%v", state, desc, err)
	}
}

func TestRunSandboxDockerUnavailable(t *testing.T) {
	f := &fakeSandbox{unavailable: errors.New("Docker 不可用")}
	sec, state, _, err := runSandbox(context.Background(), sandboxConfig(t, true), f)
	if err == nil || state != "error" {
		t.Fatalf("Docker 不可用应返回 error: state=%s err=%v", state, err)
	}
	if len(f.specs) != 0 {
		t.Fatal("Docker 不可用时不应尝试运行容器")
	}
	if len(sec.Steps) != 1 || sec.Steps[0].Status != review.StepError || !strings.Contains(sec.Steps[0].Detail, "Docker") {
		t.Fatalf("总评里应说明具体错误: %+v", sec.Steps)
	}
}

func TestRunSandboxNoCommandsConfigured(t *testing.T) {
	// 既不是 Go 项目，也没有配置命令：必须报错，不能当作“通过”
	_, state, _, err := runSandbox(context.Background(), sandboxConfig(t, false), &fakeSandbox{})
	if !errors.Is(err, sandbox.ErrNoCommands) || state != "error" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestRunSandboxNeverSeesSecrets(t *testing.T) {
	cfg := sandboxConfig(t, true)
	cfg.GitHubToken, cfg.APIKey = "ghs_SECRET", "sk-SECRET"
	f := &fakeSandbox{}
	if _, _, _, err := runSandbox(context.Background(), cfg, f); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.specs {
		for _, arg := range sandbox.BuildArgs(s) {
			if strings.Contains(arg, "SECRET") {
				t.Fatalf("容器参数里出现了密钥: %q", arg)
			}
		}
		if len(s.Env) != 0 {
			t.Fatalf("不应向容器传递环境变量: %v", s.Env)
		}
	}
}

func TestToSectionMapsResults(t *testing.T) {
	rep := sandbox.Report{Image: "i", Memory: "1g", CPUs: "1", Network: "none", Elapsed: time.Second, Results: []sandbox.Result{
		{Name: "test", Status: sandbox.Failed, ExitCode: 2, Output: "out", Truncated: true, Duration: time.Second},
	}}
	s := toSection(rep)
	if s.Image != "i" || len(s.Steps) != 1 {
		t.Fatalf("%+v", s)
	}
	st := s.Steps[0]
	if st.Name != "test" || st.Status != review.StepFailed || st.Detail != "退出码 2" || st.Output != "out" || !st.Truncated {
		t.Fatalf("%+v", st)
	}
}
