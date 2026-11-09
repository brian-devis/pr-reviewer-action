//go:build integration

// 这些测试会真实地调用 Docker，只在带 -tags integration 时运行（CI 里的 ubuntu runner 自带 Docker）：
//
//	go test -tags integration -timeout 20m ./internal/sandbox/
package sandbox

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const alpine = "alpine:3.20"

func run(t *testing.T, mutate func(*Spec)) Result {
	t.Helper()
	work := t.TempDir()
	s := Spec{
		Name: "prr-it-" + randomHex(6), Image: alpine, Command: "true",
		Memory: "256m", CPUs: "1", Timeout: 2 * time.Minute, Network: "none", WorkDir: work,
	}
	if err := exec.Command("chmod", "777", work).Run(); err != nil {
		t.Fatal(err)
	}
	mutate(&s)
	return Docker{}.Run(context.Background(), s)
}

func TestIntegrationDockerAvailable(t *testing.T) {
	if err := (Docker{}).Available(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationNetworkIsDisabled(t *testing.T) {
	// 先确认这个 wget 在有网络时能成功，否则“失败”不能说明断网生效
	online := run(t, func(s *Spec) {
		s.Network = "bridge"
		s.Command = "wget -T 10 -q -O /dev/null http://example.com"
	})
	if online.Status != Passed {
		t.Skipf("runner 本身不能访问外网，无法对比: %+v", online)
	}
	offline := run(t, func(s *Spec) { s.Command = "wget -T 5 -q -O /dev/null http://example.com" })
	if offline.Status != Failed {
		t.Fatalf("断网容器不应访问到公网，got %+v", offline)
	}
	ip := run(t, func(s *Spec) { s.Command = "wget -T 5 -q -O /dev/null http://1.1.1.1" })
	if ip.Status != Failed {
		t.Fatalf("断网容器不应访问到公网 IP，got %+v", ip)
	}
}

func TestIntegrationNoHostSecretsVisible(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghs_SUPERSECRETTOKEN")
	t.Setenv("INPUT_API_KEY", "sk-SUPERSECRETKEY")
	t.Setenv("INPUT_GITHUB_TOKEN", "ghs_SUPERSECRETTOKEN2")
	res := run(t, func(s *Spec) { s.Command = "env" })
	if res.Status != Passed {
		t.Fatalf("%+v", res)
	}
	for _, leak := range []string{"SUPERSECRET", "GITHUB_TOKEN", "INPUT_API_KEY"} {
		if strings.Contains(res.Output, leak) {
			t.Fatalf("容器里能看到宿主机的 %q:\n%s", leak, res.Output)
		}
	}
}

func TestIntegrationNoDockerSocket(t *testing.T) {
	res := run(t, func(s *Spec) { s.Command = "test ! -e /var/run/docker.sock" })
	if res.Status != Passed {
		t.Fatalf("容器里不应有 docker.sock: %+v", res)
	}
}

func TestIntegrationRunsAsNonRoot(t *testing.T) {
	res := run(t, func(s *Spec) { s.Command = "id -u" })
	if strings.TrimSpace(res.Output) != "65534" {
		t.Fatalf("应以 65534 运行，got %q", res.Output)
	}
}

func TestIntegrationReadOnlyRootButTmpWritable(t *testing.T) {
	res := run(t, func(s *Spec) { s.Command = "touch /tmp/ok && ! touch /etc/blocked" })
	if res.Status != Passed {
		t.Fatalf("根文件系统应只读、/tmp 应可写: %+v", res)
	}
}

func TestIntegrationCapabilitiesDropped(t *testing.T) {
	// CapEff 全 0 表示没有任何 capability
	res := run(t, func(s *Spec) { s.Command = "grep CapEff /proc/self/status" })
	if !strings.Contains(res.Output, "0000000000000000") {
		t.Fatalf("应去掉所有 capabilities: %q", res.Output)
	}
}

func TestIntegrationFailingCommandIsFailedNotError(t *testing.T) {
	res := run(t, func(s *Spec) { s.Command = "echo boom; exit 3" })
	if res.Status != Failed || res.ExitCode != 3 || !strings.Contains(res.Output, "boom") {
		t.Fatalf("got %+v", res)
	}
}

func TestIntegrationOOMIsDetected(t *testing.T) {
	res := run(t, func(s *Spec) {
		s.Memory = "64m"
		// 字符串每轮翻倍，很快超出内存限制
		s.Command = `awk 'BEGIN{s="x"; while(1) s = s s}'`
	})
	if !res.OOM || res.Status != Errored {
		t.Fatalf("应识别为 OOM 且状态为 error，got %+v", res)
	}
}

func TestIntegrationTimeoutKillsAndRemovesContainer(t *testing.T) {
	name := "prr-it-timeout-" + randomHex(6)
	start := time.Now()
	res := run(t, func(s *Spec) {
		s.Name = name
		s.Timeout = 3 * time.Second
		s.Command = "while :; do :; done"
	})
	if !res.TimedOut || res.Status != Errored {
		t.Fatalf("应识别为超时且状态为 error，got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Second {
		t.Fatalf("超时后应很快返回，实际 %s", elapsed)
	}
	out, err := exec.Command("docker", "ps", "-a", "-q", "--filter", "name="+name).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("超时后容器应被清理，但仍然存在: %s", out)
	}
}

func TestIntegrationBadImageIsErrorAndCleanedUp(t *testing.T) {
	res := run(t, func(s *Spec) {
		s.Image = "prr-this-image-does-not-exist.invalid/nothing:0"
		s.Timeout = 60 * time.Second
	})
	if res.Status != Errored || res.Err == nil {
		t.Fatalf("镜像不存在应是沙盒错误（error），got %+v", res)
	}
}

func TestIntegrationGitDirIsNotInWorkspace(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, ".git", "config"), "extraheader = AUTHORIZATION: basic SECRET")
	write(t, filepath.Join(src, "file.txt"), "hello")

	rep, err := Run(context.Background(), Docker{}, Options{
		Source: src, Image: alpine, Memory: "256m", CPUs: "1", Timeout: time.Minute,
		TestCmd: "test ! -e .git && test -f file.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if Overall(rep.Results) != Passed {
		t.Fatalf("容器里不应看到 .git: %+v", rep.Results)
	}
}

// depProject 创建一个带第三方依赖的 Go 模块。依赖只能在联网的准备阶段下载，
// 之后的 test / lint 都是断网的，所以能跑通就证明“准备依赖 → 断网运行”的流程是对的。
func depProject(t *testing.T, testBody string) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/sandboxdemo\n\ngo 1.22\n\nrequire github.com/google/uuid v1.6.0\n")
	write(t, filepath.Join(dir, "demo.go"), `package demo

import "github.com/google/uuid"

func ID() string { return uuid.NewString() }
`)
	write(t, filepath.Join(dir, "demo_test.go"), `package demo

import "testing"

func TestID(t *testing.T) {
`+testBody+`
}
`)
	// 真实项目都会提交 go.sum；没有它，断网后 go 会（正确地）报 missing go.sum entry。
	// 在宿主机上用 go mod tidy 生成，这是受信任的测试夹具，不经过沙盒。
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Skipf("无法在宿主机生成 go.sum（可能没有网络）: %v\n%s", err, out)
	}
	return dir
}

func TestIntegrationGoProjectWithDependenciesPasses(t *testing.T) {
	dir := depProject(t, `if len(ID()) != 36 { t.Fatal("bad id") }`)
	opts, err := Resolve(Options{Source: dir, Memory: "1g", CPUs: "1", Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Docker{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		t.Logf("%s: %s (%s)\n%s", r.Name, r.Status, r.Detail(), r.Output)
	}
	if state, _ := Describe(rep.Results); state != "success" {
		t.Fatalf("带依赖的项目在断网下应能通过测试和 vet，state=%s", state)
	}
}

func TestIntegrationGoProjectFailingTestStillRunsLint(t *testing.T) {
	dir := depProject(t, `t.Fatal("intentional failure")`)
	opts, _ := Resolve(Options{Source: dir, Memory: "1g", CPUs: "1", Timeout: 5 * time.Minute})
	rep, err := Run(context.Background(), Docker{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("应同时有 test 和 lint 结果: %+v", rep.Results)
	}
	if rep.Results[0].Status != Failed || rep.Results[1].Status != Passed {
		t.Fatalf("test 应失败、lint 应通过: %s / %s", rep.Results[0].Status, rep.Results[1].Status)
	}
	if state, _ := Describe(rep.Results); state != "failure" {
		t.Fatalf("state = %s", state)
	}
}

// nodeProjectWithDep 创建一个带第三方依赖的 Node 项目，package.json 里故意放一个
// 会写文件的 postinstall 脚本。依赖只能在联网的 npm --ignore-scripts 阶段下载，
// 之后断网运行，所以能跑通就证明“下载依赖 → 不执行安装脚本 → 断网运行”是对的。
func nodeProjectWithDep(t *testing.T) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{
  "name": "sandboxdemo",
  "version": "1.0.0",
  "private": true,
  "scripts": {
    "test": "node demo_test.js",
    "lint": "node --check demo.js",
    "postinstall": "node -e \"require('fs').writeFileSync('INSTALL_SCRIPT_RAN','x')\""
  },
  "dependencies": { "left-pad": "1.3.0" }
}
`)
	write(t, filepath.Join(dir, "demo.js"), `const leftPad = require('left-pad');
module.exports = (s) => leftPad(s, 5, '0');
`)
	write(t, filepath.Join(dir, "demo_test.js"), `const fs = require('fs');
const pad = require('./demo');
if (pad('42') !== '00042') { console.error('bad padding: ' + pad('42')); process.exit(1); }
// postinstall 脚本绝不能执行：否则等于在准备阶段运行了 PR 依赖里的任意代码
if (fs.existsSync('INSTALL_SCRIPT_RAN')) { console.error('postinstall 执行了'); process.exit(1); }
`)
	return dir
}

func TestIntegrationNodeProjectWithDependenciesPasses(t *testing.T) {
	dir := nodeProjectWithDep(t)
	opts, err := Resolve(Options{Source: dir, Memory: "1g", CPUs: "1", Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Docker{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Kind != KindNode {
		t.Fatalf("应识别为 Node 项目: %s", rep.Kind)
	}
	for _, r := range rep.Results {
		t.Logf("%s: %s (%s)\n%s", r.Name, r.Status, r.Detail(), r.Output)
	}
	if state, _ := Describe(rep.Results); state != "success" {
		t.Fatalf("带依赖的 Node 项目在断网下应能通过 test 和 lint，state=%s", state)
	}
}

func TestIntegrationPythonProjectWithDependenciesPasses(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "requirements.txt"), "six==1.16.0\n")
	write(t, filepath.Join(dir, "demo.py"), `import six


def double(x):
    return six.text_type(x) * 2
`)
	write(t, filepath.Join(dir, "demo_test.py"), `from demo import double

assert double("ab") == "abab", double("ab")
`)
	opts, err := Resolve(Options{Source: dir, Memory: "1g", CPUs: "1", Timeout: 5 * time.Minute,
		TestCmd: `python -m unittest discover -v -p "*_test.py"`})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Run(context.Background(), Docker{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Results {
		t.Logf("%s: %s (%s)\n%s", r.Name, r.Status, r.Detail(), r.Output)
	}
	if state, _ := Describe(rep.Results); state != "success" {
		t.Fatalf("带依赖的 Python 项目在断网下应能通过 test 和 lint，state=%s", state)
	}
}
