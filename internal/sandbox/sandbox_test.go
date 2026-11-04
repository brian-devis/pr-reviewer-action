package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func baseSpec() Spec {
	return Spec{
		Name: "prr-test-abc", Image: "golang:1.22", Command: "go test ./...",
		Memory: "1g", CPUs: "1", Timeout: time.Minute, Network: "none", WorkDir: "/tmp/w",
	}
}

func has(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j, w := range want {
			if args[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestBuildArgsSecurityFlags(t *testing.T) {
	args := BuildArgs(baseSpec())
	for _, want := range [][]string{
		{"--network", "none"},
		{"--memory", "1g"},
		{"--memory-swap", "1g"},
		{"--cpus", "1"},
		{"--pids-limit", "256"},
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--user", "65534:65534"},
		{"--read-only"},
	} {
		if !has(args, want...) {
			t.Errorf("缺少安全参数 %v\n完整参数: %v", want, args)
		}
	}
}

func TestBuildArgsCommandIsSingleShellArgument(t *testing.T) {
	// 命令里即使带有看起来像 docker 选项的内容，也必须作为 sh -c 的单个参数，
	// 排在镜像名之后，不会被 docker 解析成选项。
	s := baseSpec()
	s.Command = "--privileged -v /:/host; echo hi"
	args := BuildArgs(s)
	n := len(args)
	if args[n-4] != "golang:1.22" || args[n-3] != "sh" || args[n-2] != "-c" || args[n-1] != s.Command {
		t.Fatalf("尾部应为 镜像 sh -c 命令，got %v", args[n-4:])
	}
	for _, a := range args[:n-4] {
		if a == "--privileged" {
			t.Fatal("命令内容泄漏到了 docker 选项里")
		}
	}
}

func TestBuildArgsNeverForwardsHostSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_secret")
	t.Setenv("INPUT_API_KEY", "sk-secret")
	for _, a := range BuildArgs(baseSpec()) {
		if strings.Contains(a, "ghs_secret") || strings.Contains(a, "sk-secret") {
			t.Fatalf("参数里出现了宿主机密钥: %q", a)
		}
	}
	// 没有 --env-file，也没有不带值的 --env NAME（后者会从宿主机继承变量）
	args := BuildArgs(baseSpec())
	for i, a := range args {
		if a == "--env-file" {
			t.Fatal("不应使用 --env-file")
		}
		if a == "--env" && !strings.Contains(args[i+1], "=") {
			t.Fatalf("--env %s 没有显式值，会继承宿主机变量", args[i+1])
		}
	}
}

func TestBuildArgsOfflineSetsGoOffline(t *testing.T) {
	args := BuildArgs(baseSpec())
	if !has(args, "--env", "GOPROXY=off") || !has(args, "--env", "GOFLAGS=-mod=readonly") {
		t.Fatalf("断网时应让 go 命令立刻失败而不是卡住: %v", args)
	}
	s := baseSpec()
	s.Network = "bridge"
	if has(BuildArgs(s), "--env", "GOPROXY=off") {
		t.Fatal("联网准备阶段不应设置 GOPROXY=off")
	}
}

func TestBuildArgsModCacheMount(t *testing.T) {
	s := baseSpec()
	s.ModCache = "/tmp/m"
	if !has(BuildArgs(s), "--mount", "type=bind,source=/tmp/m,target=/gomod,readonly") {
		t.Fatalf("测试阶段模块缓存应只读挂载: %v", BuildArgs(s))
	}
	s.ModCacheWritable = true
	if !has(BuildArgs(s), "--mount", "type=bind,source=/tmp/m,target=/gomod") {
		t.Fatalf("准备阶段模块缓存应可写: %v", BuildArgs(s))
	}
	if !has(BuildArgs(baseSpec()), "--workdir", "/work") {
		t.Fatal("应以 /work 为工作目录")
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		r    Result
		want Status
	}{
		{"通过", Result{}, Passed},
		{"命令失败", Result{ExitCode: 1}, Failed},
		{"OOM 是 error", Result{ExitCode: 137, OOM: true}, Errored},
		{"超时是 error", Result{ExitCode: 137, TimedOut: true}, Errored},
		{"docker 错误是 error", Result{Err: errors.New("x")}, Errored},
	}
	for _, tt := range tests {
		if got := Classify(tt.r); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestOverallAndDescribe(t *testing.T) {
	p, f, e := Result{Status: Passed}, Result{Status: Failed}, Result{Status: Errored}
	tests := []struct {
		name  string
		in    []Result
		state string
		desc  string
	}{
		{"全部通过", []Result{p, p}, "success", "All sandbox checks passed"},
		{"有失败", []Result{p, f}, "failure", "Sandbox checks failed"},
		{"有错误优先于失败", []Result{f, e}, "error", "Sandbox execution error"},
		{"没有步骤算错误", nil, "error", "Sandbox execution error"},
	}
	for _, tt := range tests {
		state, desc := Describe(tt.in)
		if state != tt.state || desc != tt.desc {
			t.Errorf("%s: got %s/%q", tt.name, state, desc)
		}
		for _, r := range desc {
			if r > 0xFFFF {
				t.Errorf("描述含 4 字节字符: %q", desc)
			}
		}
	}
}

func TestTailBufferKeepsEnd(t *testing.T) {
	b := newTailBuffer(10)
	b.Write([]byte("0123456789"))
	b.Write([]byte("abcde"))
	got, truncated := b.String()
	if got != "56789abcde" || !truncated {
		t.Fatalf("got %q truncated=%v", got, truncated)
	}
	small := newTailBuffer(100)
	small.Write([]byte("hi"))
	if got, tr := small.String(); got != "hi" || tr {
		t.Fatalf("got %q truncated=%v", got, tr)
	}
}

func TestTailBufferConcurrentWrites(t *testing.T) {
	b := newTailBuffer(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				b.Write([]byte("line of output\n"))
			}
		}()
	}
	wg.Wait()
	if got, _ := b.String(); len(got) > 1000 {
		t.Fatalf("超出上限: %d", len(got))
	}
}

func TestCleanOutputRepairsCutRunes(t *testing.T) {
	// 截断从“你”的中间开始：残缺的字节被去掉，完整的“好”保留，NUL 被去掉
	cut := string([]byte("你好")[1:]) + "ok\x00"
	got := cleanOutput(cut)
	if strings.ContainsRune(got, 0) || got != "好ok" {
		t.Fatalf("got %q", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopyWorkspaceExcludesGitAndKeepsRest(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, ".git", "config"), "[http]\n\textraheader = AUTHORIZATION: basic SECRET")
	write(t, filepath.Join(src, "main.go"), "package main")
	write(t, filepath.Join(src, "pkg", "a.go"), "package pkg")
	write(t, filepath.Join(src, ".github", "workflows", "ci.yml"), "name: ci")

	if err := CopyWorkspace(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git 必须被排除，里面可能有 checkout 留下的 GitHub Token")
	}
	for _, f := range []string{"main.go", "pkg/a.go", ".github/workflows/ci.yml"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("缺少 %s: %v", f, err)
		}
	}
}

func TestCopyWorkspaceExcludesNestedGitOnlyAtRoot(t *testing.T) {
	// 只排除仓库根目录的 .git；子目录里同名的普通目录属于项目内容
	src, dst := t.TempDir(), t.TempDir()
	write(t, filepath.Join(src, "testdata", ".git", "keep.txt"), "fixture")
	if err := CopyWorkspace(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "testdata", ".git", "keep.txt")); err != nil {
		t.Fatalf("子目录里的 .git 不应被排除: %v", err)
	}
}

func TestCopyWorkspaceSymlinkNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建符号链接需要特权")
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "TOP SECRET")
	src, dst := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := CopyWorkspace(src, dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dst, "escape"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("应保持为符号链接而不是复制目标内容: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "escape", "secret.txt")); err == nil {
		// 链接在宿主机上仍然能解析到原目录；关键是内容没有被复制进副本
		if entries, _ := os.ReadDir(dst); len(entries) != 1 {
			t.Fatalf("目标内容被复制进了副本: %v", entries)
		}
	}
}

func TestGoVersion(t *testing.T) {
	dir := t.TempDir()
	if GoVersion(dir) != "" {
		t.Fatal("没有 go.mod 应返回空")
	}
	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22.5\n\ntoolchain go1.23.1\n")
	if got := GoVersion(dir); got != "1.22" {
		t.Fatalf("got %q", got)
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(Options{Source: dir}); !errors.Is(err, ErrNoCommands) {
		t.Fatalf("非 Go 项目且没有命令应报错: %v", err)
	}

	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.21\n")
	o, err := Resolve(Options{Source: dir})
	if err != nil || o.Image != "golang:1.21" || o.TestCmd != "go test ./..." || o.LintCmd != "go vet ./..." {
		t.Fatalf("Go 项目默认值错误: %+v err=%v", o, err)
	}

	o, _ = Resolve(Options{Source: dir, Image: "golang:1.25", TestCmd: "make test"})
	if o.Image != "golang:1.25" || o.TestCmd != "make test" || o.LintCmd != "" {
		t.Fatalf("显式配置不应被覆盖，只配置 test 时不应补 lint: %+v", o)
	}
}

// fakeRunner 记录每次运行，并按名称返回预设结果。
type fakeRunner struct {
	mu      sync.Mutex
	specs   []Spec
	results map[string]Result
}

func (f *fakeRunner) Run(_ context.Context, s Spec) Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, s)
	for key, r := range f.results {
		if strings.HasPrefix(s.Name, "prr-"+key+"-") {
			r.Status = Classify(r)
			return r
		}
	}
	return Result{Status: Passed}
}

func goProject(t *testing.T) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "a.go"), "package x")
	return dir
}

func TestRunStepsAndIsolation(t *testing.T) {
	f := &fakeRunner{}
	o, _ := Resolve(Options{Source: goProject(t), Memory: "1g", CPUs: "1", Timeout: time.Minute})
	rep, err := Run(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 3 {
		t.Fatalf("应有 deps、test、lint 三次容器运行，got %d", len(f.specs))
	}
	deps, test, lint := f.specs[0], f.specs[1], f.specs[2]

	if deps.Network != "bridge" || !deps.ModCacheWritable || deps.Command != "go mod download" {
		t.Errorf("准备阶段应联网并可写模块缓存: %+v", deps)
	}
	for _, s := range []Spec{test, lint} {
		if s.Network != "none" || s.ModCacheWritable {
			t.Errorf("运行阶段必须断网且模块缓存只读: %+v", s)
		}
	}
	if test.Name == lint.Name || test.WorkDir != lint.WorkDir {
		t.Errorf("test 和 lint 应是不同容器、共用同一份临时副本: %q %q", test.Name, lint.Name)
	}
	if test.WorkDir == o.Source {
		t.Error("容器必须使用临时副本，不能直接挂载原始检出目录")
	}
	if len(rep.Results) != 2 || rep.Results[0].Name != "test" || rep.Results[1].Name != "lint" {
		t.Fatalf("结果应只含 test 和 lint: %+v", rep.Results)
	}
	if _, err := os.Stat(test.WorkDir); !os.IsNotExist(err) {
		t.Error("运行结束后临时副本应被清理")
	}
}

func TestRunContinuesLintAfterTestFails(t *testing.T) {
	f := &fakeRunner{results: map[string]Result{"test": {ExitCode: 1}}}
	o, _ := Resolve(Options{Source: goProject(t), Memory: "1g", CPUs: "1", Timeout: time.Minute})
	rep, _ := Run(context.Background(), f, o)
	if len(rep.Results) != 2 {
		t.Fatalf("Test 失败后仍应运行 Lint: %+v", rep.Results)
	}
	if rep.Results[0].Status != Failed || rep.Results[1].Status != Passed {
		t.Fatalf("got %s / %s", rep.Results[0].Status, rep.Results[1].Status)
	}
	if state, _ := Describe(rep.Results); state != "failure" {
		t.Fatalf("state = %s", state)
	}
}

func TestRunStopsWhenDependencyPrepFails(t *testing.T) {
	f := &fakeRunner{results: map[string]Result{"deps": {ExitCode: 1}}}
	o, _ := Resolve(Options{Source: goProject(t), Memory: "1g", CPUs: "1", Timeout: time.Minute})
	rep, _ := Run(context.Background(), f, o)
	if len(f.specs) != 1 {
		t.Fatalf("准备失败后不应再运行 test/lint，实际运行了 %d 个容器", len(f.specs))
	}
	if len(rep.Results) != 1 || rep.Results[0].Status != Errored {
		t.Fatalf("准备失败应是 error 而不是 failed: %+v", rep.Results)
	}
}

func TestRunNonGoProjectSkipsDependencyPrep(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Makefile"), "test:\n\techo ok")
	f := &fakeRunner{}
	o, err := Resolve(Options{Source: dir, Image: "alpine:3", TestCmd: "echo ok", Memory: "1g", CPUs: "1", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), f, o); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 1 || f.specs[0].Network != "none" || f.specs[0].ModCache != "" {
		t.Fatalf("非 Go 项目不应联网准备依赖: %+v", f.specs)
	}
}
