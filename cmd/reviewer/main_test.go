package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestSandboxExitError(t *testing.T) {
	tests := []struct {
		name       string
		failOn     string
		state      string
		sandboxErr error
		want       bool
	}{
		{"never 下失败也不让步骤失败", "never", review.StateFailure, nil, false},
		{"failure 命中测试没通过", "failure", review.StateFailure, nil, true},
		{"failure 不覆盖执行异常", "failure", review.StateError, nil, false},
		{"error 命中执行异常", "error", review.StateError, nil, true},
		{"error 不覆盖测试没通过", "error", review.StateFailure, nil, false},
		{"any 两者都命中", "any", review.StateFailure, nil, true},
		{"any 覆盖执行异常", "any", review.StateError, nil, true},
		{"通过时不失败", "any", review.StateSuccess, nil, false},
		// 沙盒本身跑不起来时由调用方已有的 err 负责，这里不重复报告
		{"沙盒错误已存在时不重复", "any", review.StateError, errors.New("docker unavailable"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{SandboxFailOn: tt.failOn}
			got := sandboxExitError(cfg, tt.state, "desc", tt.sandboxErr) != nil
			if got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// reviewServer 起一个假的 GitHub API，记录提交的行内 review。
type reviewServer struct {
	posted [][]ghclient.ReviewComment
}

func newReviewServer(t *testing.T) (*ghclient.Client, *reviewServer, *httptest.Server) {
	t.Helper()
	rs := &reviewServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
			w.Write([]byte(`diff --git a/a.go b/a.go
index 1111111..2222222 100644
--- a/a.go
+++ b/a.go
@@ -1,2 +1,3 @@
 package a
+var x = 1
 func f() {}
`))
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/repos/o/r/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/o/r/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Comments []ghclient.ReviewComment `json:"comments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析 review 请求失败: %v", err)
		}
		rs.posted = append(rs.posted, payload.Comments)
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &ghclient.Client{Token: "t", API: srv.URL, Repo: "o/r", HTTP: srv.Client()}, rs, srv
}

func inlineTestConfig() *config.Config {
	return &config.Config{
		PRNumber: 1, HeadSHA: "sha", Model: "m",
		MaxDiffBytes: 60000, MaxRequests: 5,
		FailOn: "error", InlineMinSeverity: "warning",
	}
}

func TestExecuteFiltersInlineByThresholdWithoutAffectingGate(t *testing.T) {
	reply := `{"summary":"概要","comments":[
		{"file":"a.go","line":2,"severity":"error","message":"真问题"},
		{"file":"a.go","line":2,"severity":"info","message":"低价值建议"}]}`

	gh, rs, _ := newReviewServer(t)
	res, err := execute(context.Background(), inlineTestConfig(), gh, &fakeProvider{replies: []string{reply}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.report.Inline) != 1 || len(res.report.Deferred) != 1 {
		t.Fatalf("inline=%d deferred=%d", len(res.report.Inline), len(res.report.Deferred))
	}
	if res.report.Deferred[0].Message != "低价值建议" {
		t.Fatalf("被推迟的应是 info: %+v", res.report.Deferred)
	}
	// 只有达到门槛的评论被提交为行内评论
	if len(rs.posted) != 1 || len(rs.posted[0]) != 1 || !strings.Contains(rs.posted[0][0].Body, "真问题") {
		t.Fatalf("行内评论提交内容不对: %+v", rs.posted)
	}
	// 门槛不改变门禁：error 级问题仍然让 fail_on=error 失败
	if res.state != review.StateFailure {
		t.Fatalf("state=%s，error 级问题应触发 failure", res.state)
	}

	// 只有 info 时：行内评论一条都不发，但状态仍是 success（fail_on=error）
	infoOnly := `{"summary":"概要","comments":[{"file":"a.go","line":2,"severity":"info","message":"低价值建议"}]}`
	gh2, rs2, _ := newReviewServer(t)
	res2, err := execute(context.Background(), inlineTestConfig(), gh2, &fakeProvider{replies: []string{infoOnly}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.report.Inline) != 0 || len(res2.report.Deferred) != 1 {
		t.Fatalf("inline=%d deferred=%d", len(res2.report.Inline), len(res2.report.Deferred))
	}
	if len(rs2.posted) != 0 {
		t.Fatalf("低于门槛的评论不应提交行内 review: %+v", rs2.posted)
	}
	if res2.state != review.StateSuccess {
		t.Fatalf("state=%s，info 不应触发 fail_on=error", res2.state)
	}

	// 门槛设为 info 时恢复旧行为：全部发布为行内评论
	cfg := inlineTestConfig()
	cfg.InlineMinSeverity = "info"
	gh3, rs3, _ := newReviewServer(t)
	res3, err := execute(context.Background(), cfg, gh3, &fakeProvider{replies: []string{infoOnly}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res3.report.Deferred) != 0 || len(rs3.posted) != 1 || len(rs3.posted[0]) != 1 {
		t.Fatalf("threshold=info 时应全部发布: deferred=%d posted=%+v", len(res3.report.Deferred), rs3.posted)
	}
}

// fileNameInUser 从构造好的 prompt 里取出被审查的文件路径，供并发 provider 区分批次。
func fileNameInUser(user string) string {
	const marker = "### FILE: "
	i := strings.Index(user, marker)
	if i < 0 {
		return "?"
	}
	rest := user[i+len(marker):]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// concurrentProvider 是线程安全的 provider：用原子计数记录峰值并发度，并让所有请求在
// 入口处汇合，保证各批次确实同时执行后再按文件路径模拟不同耗时（第 0 批慢于第 2 批）。
type concurrentProvider struct {
	peak    atomic.Int32
	cur     atomic.Int32
	entered atomic.Int32
	total   int
	delay   map[string]time.Duration
}

func (p *concurrentProvider) Complete(ctx context.Context, req llm.Request) (string, error) {
	name := fileNameInUser(req.User)
	n := p.cur.Add(1)
	defer p.cur.Add(-1)
	for {
		pn := p.peak.Load()
		if n <= pn || p.peak.CompareAndSwap(pn, n) {
			break
		}
	}
	p.entered.Add(1)
	for p.entered.Load() < int32(p.total) {
		time.Sleep(time.Millisecond)
	}
	if d, ok := p.delay[name]; ok {
		time.Sleep(d)
	}
	return fmt.Sprintf(`{"summary":"summary-%s","comments":[{"file":"%s","line":1,"severity":"warning","message":"comment-%s"}]}`, name, name, name), nil
}

// failSomeProvider 对指定文件（批次）返回错误，其余返回正常结果。
type failSomeProvider struct {
	fail map[string]bool
}

func (p *failSomeProvider) Complete(ctx context.Context, req llm.Request) (string, error) {
	name := fileNameInUser(req.User)
	if p.fail[name] {
		return "", errors.New("context deadline exceeded")
	}
	return fmt.Sprintf(`{"summary":"summary-%s","comments":[{"file":"%s","line":1,"severity":"warning","message":"comment-%s"}]}`, name, name, name), nil
}

// multiFileDiffBody 返回包含 3 个文件的 unified diff，配合较小的 MaxDiffBytes 拆成 3 批。
func multiFileDiffBody() string {
	var b strings.Builder
	for _, name := range []string{"a.go", "b.go", "c.go"} {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\nindex 1111111..2222222 100644\n--- a/%s\n+++ b/%s\n@@ -1,1 +1,30 @@\n package %s\n", name, name, name, name, strings.TrimSuffix(name, ".go"))
		for j := 0; j < 30; j++ {
			fmt.Fprintf(&b, "+line %02d in %s\n", j, name)
		}
	}
	return b.String()
}

// newReviewServerWithDiff 与 newReviewServer 类似，但 GetDiff 返回传入的 diff。
func newReviewServerWithDiff(t *testing.T, diffBody string) (*ghclient.Client, *reviewServer, *httptest.Server) {
	t.Helper()
	rs := &reviewServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") == "application/vnd.github.v3.diff" {
			w.Write([]byte(diffBody))
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/repos/o/r/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/o/r/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Comments []ghclient.ReviewComment `json:"comments"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("解析 review 请求失败: %v", err)
		}
		rs.posted = append(rs.posted, payload.Comments)
		w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &ghclient.Client{Token: "t", API: srv.URL, Repo: "o/r", HTTP: srv.Client()}, rs, srv
}

func TestExecuteConcurrentChunksPreserveOrder(t *testing.T) {
	gh, rs, _ := newReviewServerWithDiff(t, multiFileDiffBody())
	cfg := inlineTestConfig()
	cfg.MaxDiffBytes = 200 // 每个文件单独成一批，共 3 批
	p := &concurrentProvider{total: 3, delay: map[string]time.Duration{"a.go": 200 * time.Millisecond}}

	res, err := execute(context.Background(), cfg, gh, p)
	if err != nil {
		t.Fatal(err)
	}
	if p.peak.Load() < 2 {
		t.Fatalf("峰值并发度应 >= 2，实际 %d", p.peak.Load())
	}

	want := []string{"summary-a.go", "summary-b.go", "summary-c.go"}
	if len(res.report.Summaries) != len(want) {
		t.Fatalf("摘要数量=%d，期望 %d：%v", len(res.report.Summaries), len(want), res.report.Summaries)
	}
	for i := range want {
		if res.report.Summaries[i] != want[i] {
			t.Fatalf("摘要顺序被破坏：第 %d 个=%q，期望 %q（全部=%v）", i, res.report.Summaries[i], want[i], res.report.Summaries)
		}
	}

	if len(res.report.Inline) != 3 {
		t.Fatalf("行内评论数量=%d，期望 3", len(res.report.Inline))
	}
	for i, name := range []string{"a.go", "b.go", "c.go"} {
		if res.report.Inline[i].File != name {
			t.Fatalf("行内评论顺序被破坏：第 %d 个=%s，期望 %s", i, res.report.Inline[i].File, name)
		}
	}
	if len(rs.posted) != 1 || len(rs.posted[0]) != 3 {
		t.Fatalf("提交的行内评论异常: %+v", rs.posted)
	}
	for i, name := range []string{"a.go", "b.go", "c.go"} {
		if rs.posted[0][i].Path != name {
			t.Fatalf("提交顺序被破坏：第 %d 个=%s，期望 %s", i, rs.posted[0][i].Path, name)
		}
	}
}

func TestReportingContext(t *testing.T) {
	ctx, cancel := reportingContext(nil)
	if ctx == nil || ctx.Err() != nil {
		t.Fatalf("nil parent 应返回可用上下文")
	}
	cancel()

	parent, pc := context.WithCancel(context.Background())
	pc()
	ctx, cancel = reportingContext(parent)
	if ctx == nil || ctx.Err() != nil {
		t.Fatalf("已取消 parent 应返回可用上下文")
	}
	cancel()

	parent, pc = context.WithTimeout(context.Background(), time.Millisecond)
	defer pc()
	time.Sleep(5 * time.Millisecond)
	ctx, cancel = reportingContext(parent)
	if ctx == nil || ctx.Err() != nil {
		t.Fatalf("已过期 parent 应返回可用上下文")
	}
	cancel()

	parent, pc = context.WithTimeout(context.Background(), time.Second)
	defer pc()
	ctx, cancel = reportingContext(parent)
	if ctx == nil || ctx.Err() != nil || ctx == parent {
		t.Fatalf("剩余时间不足时应派生新上下文")
	}
	cancel()

	parent, pc = context.WithTimeout(context.Background(), 5*time.Minute)
	defer pc()
	ctx, cancel = reportingContext(parent)
	if ctx != parent {
		t.Fatalf("剩余时间充足时应沿用 parent")
	}
	cancel()
}

func TestPostInlineWithExpiredContext(t *testing.T) {
	gh, rs, _ := newReviewServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("前置条件：ctx 应已取消")
	}

	inline := []review.Comment{{File: "a.go", Line: 2, Severity: "error", Message: "问题"}}
	if err := postInline(ctx, gh, inlineTestConfig(), inline); err != nil {
		t.Fatalf("过期 ctx 下应仍能提交行内评论: %v", err)
	}
	if len(rs.posted) != 1 || len(rs.posted[0]) != 1 {
		t.Fatalf("应提交 1 条行内评论: %+v", rs.posted)
	}
}

func TestExecutePartialFailureStillReports(t *testing.T) {
	gh, _, _ := newReviewServerWithDiff(t, multiFileDiffBody())
	cfg := inlineTestConfig()
	cfg.MaxDiffBytes = 200
	p := &failSomeProvider{fail: map[string]bool{"b.go": true}}

	res, err := execute(context.Background(), cfg, gh, p)
	if err != nil {
		t.Fatalf("部分批次失败不应让整个评审报错: %v", err)
	}
	if !res.reviewed {
		t.Fatal("应返回 reviewed=true")
	}
	if len(res.report.Inline) != 2 {
		t.Fatalf("应保留成功批次的 2 条行内评论，实际 %d", len(res.report.Inline))
	}
	if res.state != review.StateError {
		t.Fatalf("审查不完整应置为 error，实际 %s", res.state)
	}
	if !strings.Contains(res.desc, "不完整") {
		t.Fatalf("描述应带 incomplete 标记: %q", res.desc)
	}
}
