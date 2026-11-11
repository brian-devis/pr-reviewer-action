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
	Image       string
	TestCmd     string
	LintCmd     string
	InstallCmd  string // 联网准备依赖的命令，留空时按项目类型取默认值
	Memory      string
	CPUs        string
	Timeout     time.Duration
	Source      string // 宿主机上的仓库检出目录（GITHUB_WORKSPACE）
	Project     Kind   // 留空时自动识别
	CacheTarget string // 覆盖项目类型决定的依赖缓存挂载点
	// AlsoDetected 由 Resolve 填写：自动识别选定类型的同时，仓库里还存在的其他项目类型。
	// 只有识别结果确实决定了镜像或命令时才填，用来提示用户“可能选错了”。
	AlsoDetected []Kind
}

// Report 是一次沙盒检查的完整结果。
type Report struct {
	Kind    Kind
	Also    []Kind // 见 Options.AlsoDetected
	Context string // commit status 名称，来自 sandbox_status_context
	Image   string
	Memory  string
	CPUs    string
	Network string
	Results []Result
	Elapsed time.Duration
}

// Resolve 补全默认值：没有指定镜像时按项目类型选镜像，没有指定命令时用该项目
// 类型对应的默认 test/lint。返回补全后的配置；无法确定要执行什么时返回 ErrNoCommands。
func Resolve(o Options) (Options, error) {
	// "auto" 和空值都表示交给 Detect 判断
	auto := o.Project == "" || o.Project == "auto"
	if auto {
		o.Project = ""
	}
	// 识别结果只有在真的决定了镜像或命令时才值得提示
	influenced := o.Image == "" || (o.TestCmd == "" && o.LintCmd == "")
	p := Detect(o.Source)
	if o.Project != "" && o.Project != p.Kind {
		// 显式指定了项目类型时，只用它的默认命令和准备方式，镜像版本仍按源码目录判断
		p = projectFor(o.Project, o.Source)
	}
	o.Project = p.Kind
	if auto && influenced {
		for _, k := range Detected(o.Source) {
			if k != p.Kind {
				o.AlsoDetected = append(o.AlsoDetected, k)
			}
		}
	}
	if o.TestCmd == "" && o.LintCmd == "" {
		if p.TestCmd == "" {
			return o, ErrNoCommands
		}
		o.TestCmd, o.LintCmd = p.TestCmd, p.LintCmd
	}
	if o.Image == "" {
		o.Image = p.DefaultImage
	}
	if o.InstallCmd == "" {
		o.InstallCmd = p.InstallCmd
	}
	if o.CacheTarget == "" {
		o.CacheTarget = p.CacheTarget
	}
	return o, nil
}

// projectFor 生成本次运行使用的 Project：类型由调用方指定，版本信息仍从源码里读。
func projectFor(k Kind, dir string) Project {
	switch k {
	case KindGo:
		return Project{Kind: KindGo, Label: "Go", DefaultImage: "golang:" + defaultOr(GoVersion(dir), "1.23"),
			TestCmd: "go test ./...", LintCmd: "go vet ./...", InstallCmd: "go mod download", CacheTarget: "/gomod"}
	case KindNode:
		return Project{Kind: KindNode, Label: "Node.js", DefaultImage: "node:" + nodeVersion(dir),
			TestCmd: "npm test --if-present", LintCmd: "npm run lint --if-present",
			InstallCmd: npmInstallCmd(dir)}
	case KindPython:
		return Project{Kind: KindPython, Label: "Python", DefaultImage: "python:" + pythonVersion(dir),
			TestCmd: "python -m pytest -q", LintCmd: "python -m compileall -q .",
			InstallCmd: pipInstallCmd(dir), CacheTarget: "/deps"}
	}
	return Project{Kind: KindGeneric, Label: "通用", DefaultImage: "golang:1.23"}
}

func defaultOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Run 在沙盒里依次执行依赖准备、测试和 Lint。Test 和 Lint 各用一个独立容器，
// 即使 Test 失败也会继续跑 Lint，分别收集结果。
//
// 依赖准备是唯一联网的步骤：它只下载依赖，不执行 PR 里的代码（Go 用
// go mod download，Node 用 npm --ignore-scripts，Python 用 pip --only-binary）。
// 之后的 test 和 lint 全部断网运行。
func Run(ctx context.Context, r Runner, o Options) (Report, error) {
	start := time.Now()
	rep := Report{Kind: o.Project, Also: o.AlsoDetected, Image: o.Image, Memory: o.Memory, CPUs: o.CPUs, Network: "none"}

	work, err := os.MkdirTemp("", "prr-work-*")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(work)
	if err := CopyWorkspace(o.Source, work); err != nil {
		return rep, fmt.Errorf("复制工作副本失败: %w", err)
	}

	var cache string
	// 只有需要额外挂载依赖目录的项目（Go 的模块缓存、Python 的 --target 目录）才创建缓存；
	// Node 的 node_modules 直接装在工作副本里，不需要挂载。
	if o.InstallCmd != "" && o.CacheTarget != "" {
		if cache, err = os.MkdirTemp("", "prr-deps-*"); err != nil {
			return rep, err
		}
		defer os.RemoveAll(cache)
		if err := os.Chmod(cache, 0o777); err != nil {
			return rep, err
		}
	}

	if o.InstallCmd != "" {
		// 准备阶段联网，但同样受资源限制、不带任何密钥
		prep := r.Run(ctx, spec(o, "deps", o.InstallCmd, work, cache, "bridge", true))
		prep.Name = "deps"
		if prep.Status != Passed {
			// 没有依赖，后面的步骤必然失败且没有意义；用 Errored 表示这是环境问题
			if prep.Status == Failed {
				prep.Status = Errored
				prep.Err = fmt.Errorf("准备依赖失败（退出码 %d）", prep.ExitCode)
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
		res := r.Run(ctx, spec(o, st.name, st.cmd, work, cache, "none", false))
		res.Name = st.name
		rep.Results = append(rep.Results, res)
	}
	rep.Elapsed = time.Since(start)
	return rep, nil
}

func spec(o Options, name, cmd, work, cache, network string, cacheWritable bool) Spec {
	kind := o.Project
	if kind == "" {
		kind = KindGeneric
	}
	return Spec{
		Name:          "prr-" + name + "-" + randomHex(6),
		Kind:          kind,
		Image:         o.Image,
		Command:       cmd,
		Memory:        o.Memory,
		CPUs:          o.CPUs,
		Timeout:       o.Timeout,
		Network:       network,
		WorkDir:       work,
		CacheDir:      cache,
		CacheTarget:   o.CacheTarget,
		CacheWritable: cacheWritable,
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0"
	}
	return hex.EncodeToString(b)
}
