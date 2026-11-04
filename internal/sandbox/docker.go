package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Runner 运行一个容器并返回结果。做成接口是为了让上层逻辑可以用假实现测试。
type Runner interface {
	Run(ctx context.Context, s Spec) Result
}

// Docker 通过宿主机上的 docker 命令运行容器。不挂载 docker.sock，
// 容器内部也没有任何办法再启动别的容器。
type Docker struct {
	// Bin 是 docker 可执行文件，默认 "docker"。
	Bin string
}

func (d Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

// Available 检查 Docker 守护进程是否可用。
func (d Docker) Available(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, d.bin(), "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("Docker 不可用: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerErrorExit 是 docker run 自身失败时的退出码（守护进程错误、镜像拉取失败、参数错误等），
// 见 Docker 文档对 exit status 的约定。
const dockerErrorExit = 125

func (d Docker) Run(ctx context.Context, s Spec) Result {
	res := Result{Name: s.Name}
	start := time.Now()
	// 不管怎么退出，都要清理容器。用独立的 context，避免父 context 已取消时清理也被取消。
	defer d.remove(s.Name)

	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	tail := newTailBuffer(MaxOutput)
	cmd := exec.CommandContext(runCtx, d.bin(), BuildArgs(s)...)
	// 容器环境不继承宿主机：docker 客户端自己需要的变量由它从系统读取，
	// 容器内的环境只由 BuildArgs 里显式的 --env 决定。
	cmd.Stdout, cmd.Stderr = tail, tail
	err := cmd.Run()
	res.Duration = time.Since(start)

	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		res.Err = fmt.Errorf("无法运行 docker: %w", err)
	}

	// docker run 被杀（超时）后容器可能还活着，先强制停掉再查询状态
	if res.TimedOut {
		d.remove(s.Name)
	} else if res.Err == nil {
		res.OOM = d.detectOOM(s.Name, res.ExitCode)
	}

	out, truncated := tail.String()
	res.Output, res.Truncated = cleanOutput(out), truncated
	if res.Err == nil && res.ExitCode == dockerErrorExit && !res.TimedOut {
		res.Err = errors.New("docker 运行失败（镜像拉取失败或参数错误）: " + lastLine(res.Output))
	}
	res.Status = Classify(res)
	return res
}

// sigkillExit 是进程被 SIGKILL 杀掉时的退出码（128 + 9），OOM killer 用的就是这个信号。
const sigkillExit = 137

// detectOOM 判断容器是否因为超出内存限制被杀。
//
// docker run 返回时，容器的 State.OOMKilled 可能还没有更新（CI 里实测：立刻查询读到 false，
// 稍后再查是 true）。所以退出码是 137 时，隔一小会儿重试几次再下结论；其他退出码不可能是
// OOM 造成的，不需要等待。
func (d Docker) detectOOM(name string, exitCode int) bool {
	if d.oomKilled(name) {
		return true
	}
	if exitCode != sigkillExit {
		return false
	}
	for i := 0; i < 5; i++ {
		time.Sleep(300 * time.Millisecond)
		if d.oomKilled(name) {
			return true
		}
	}
	return false
}

// oomKilled 查询容器是否因为超出内存限制被杀。必须在容器被删除之前调用。
func (d Docker) oomKilled(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, d.bin(), "inspect", "--format", "{{.State.OOMKilled}}", name).Output()
	if err != nil {
		return false
	}
	ok, _ := strconv.ParseBool(strings.TrimSpace(string(out)))
	return ok
}

// remove 强制删除容器。容器不存在时的错误会被忽略。
func (d Docker) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, d.bin(), "rm", "-f", name).Run()
}

// cleanOutput 去掉因按字节截断可能产生的残缺 UTF-8 首字节，并去掉 NUL。
func cleanOutput(s string) string {
	s = strings.ToValidUTF8(s, "")
	if !utf8.ValidString(s) {
		return ""
	}
	return strings.ReplaceAll(s, "\x00", "")
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	return s
}
