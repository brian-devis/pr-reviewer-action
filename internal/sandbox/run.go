package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// Options 是一次沙盒检查的配置。
type Options struct {
	Image   string
	TestCmd string
	LintCmd string
	Memory  string
	CPUs    string
	Timeout time.Duration
	Source  string // 宿主机上的仓库检出目录（GITHUB_WORKSPACE）
}

// Report 是一次沙盒检查的完整结果。
type Report struct {
	Image   string
	Memory  string
	CPUs    string
	Network string
	Results []Result
	Elapsed time.Duration
}

// Resolve 补全默认值：没有指定镜像时按 go.mod 选 golang 镜像，没有指定命令时
// 对 Go 项目使用 go test / go vet。返回补全后的配置；无法确定要执行什么时返回 ErrNoCommands。
func Resolve(o Options) (Options, error) {
	isGo := HasFile(o.Source, "go.mod")
	if o.TestCmd == "" && o.LintCmd == "" {
		if !isGo {
			return o, ErrNoCommands
		}
		o.TestCmd, o.LintCmd = "go test ./...", "go vet ./..."
	}
	if o.Image == "" {
		v := GoVersion(o.Source)
		if !isGo || v == "" {
			v = "1.23"
		}
		o.Image = "golang:" + v
	}
	return o, nil
}

// Run 在沙盒里依次执行依赖准备、测试和 Lint。Test 和 Lint 各用一个独立容器，
// 即使 Test 失败也会继续跑 Lint，分别收集结果。
func Run(ctx context.Context, r Runner, o Options) (Report, error) {
	start := time.Now()
	rep := Report{Image: o.Image, Memory: o.Memory, CPUs: o.CPUs, Network: "none"}

	work, err := os.MkdirTemp("", "prr-work-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(work)
	if err := CopyWorkspace(o.Source, work); err != nil {
		return rep, fmt.Errorf("复制工作副本失败: %w", err)
	}

	var modCache string
	if HasFile(work, "go.mod") {
		if modCache, err = os.MkdirTemp("", "prr-gomod-*"); err != nil {
			return rep, err
		}
		defer os.RemoveAll(modCache)
		if err := os.Chmod(modCache, 0o777); err != nil {
			return rep, err
		}

		// 准备阶段联网，但同样受资源限制、不带任何密钥；go mod download 只下载
		// 模块，不会执行 PR 里的代码。
		prep := r.Run(ctx, spec(o, "deps", "go mod download", work, modCache, "bridge", true))
		prep.Name = "deps"
		if prep.Status != Passed {
			// 没有依赖，后面的步骤必然失败且没有意义；用 Errored 表示这是环境问题
			if prep.Status == Failed {
				prep.Status = Errored
				prep.Err = fmt.Errorf("准备 Go 依赖失败（退出码 %d）", prep.ExitCode)
			}
			rep.Results = append(rep.Results, prep)
			rep.Elapsed = time.Since(start)
			return rep, nil
		}
	}

	steps := []struct{ name, cmd string }{{"test", o.TestCmd}, {"lint", o.LintCmd}}
	for _, st := range steps {
		if st.cmd == "" {
			continue
		}
		res := r.Run(ctx, spec(o, st.name, st.cmd, work, modCache, "none", false))
		res.Name = st.name
		rep.Results = append(rep.Results, res)
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

func spec(o Options, name, cmd, work, modCache, network string, modWritable bool) Spec {
	return Spec{
		Name:             "prr-" + name + "-" + randomHex(6),
		Image:            o.Image,
		Command:          cmd,
		Memory:           o.Memory,
		CPUs:             o.CPUs,
		Timeout:          o.Timeout,
		Network:          network,
		WorkDir:          work,
		ModCache:         modCache,
		ModCacheWritable: modWritable,
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0"
	}
	return hex.EncodeToString(b)
}
