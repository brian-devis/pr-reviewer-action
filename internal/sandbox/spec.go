// Package sandbox 在 Docker 容器里执行固定配置的测试和 Lint 命令。
//
// 容器断网、限制内存和 CPU、去掉所有 Linux capabilities、用非 root 用户运行，
// 并且不继承宿主机的任何环境变量，因此拿不到 GitHub Token 或模型 API Key。
// 命令只来自 workflow 配置，绝不接受模型生成的内容。
package sandbox

import (
	"fmt"
	"strconv"
	"time"
)

// Spec 描述一次容器运行。
type Spec struct {
	Name    string // 容器名，用于清理和查询 OOM 状态
	Kind    Kind   // 项目类型，决定要不要注入 Go 专用的环境变量
	Image   string
	Command string // 交给 sh -c 执行
	Memory  string // 如 1g
	CPUs    string // 如 1 或 0.5
	Timeout time.Duration
	// Network 为 "none" 表示断网；准备依赖的阶段才会使用 "bridge"。
	Network string
	// WorkDir 是宿主机上的工作副本，挂载到容器的 /work。
	WorkDir string
	// CacheDir 是宿主机上的依赖缓存目录，挂载到容器的 CacheTarget。为空则不挂载。
	// Go 是模块缓存（/gomod），Python 是 pip --target 目录（/deps）；
	// Node 的 node_modules 在工作副本里，不需要额外挂载。
	CacheDir    string
	CacheTarget string
	// CacheWritable 只在联网准备依赖的阶段为 true，之后的运行都是只读。
	CacheWritable bool
	// Env 是显式传入的环境变量。容器不会继承宿主机环境，只有这里列出的才可见。
	Env map[string]string
}

const (
	// ContainerUser 是容器内运行命令的用户：nobody。
	ContainerUser = "65534:65534"
	pidsLimit     = "256"
	tmpSize       = "512m"
)

// BuildArgs 拼出 docker run 的完整参数。所有安全相关的开关都集中在这里，
// 方便审查和单元测试。命令作为单个参数传给 sh -c，不会被 docker 当成选项解析。
func BuildArgs(s Spec) []string {
	mem := s.Memory
	args := []string{
		"run",
		"--name", s.Name,
		"--network", s.Network,
		"--memory", mem,
		"--memory-swap", mem, // 与 memory 相同，禁止使用 swap
		"--cpus", s.CPUs,
		"--pids-limit", pidsLimit,
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,size=" + tmpSize,
		"--user", ContainerUser,
		"--workdir", "/work",
		"--mount", "type=bind,source=" + s.WorkDir + ",target=/work",
		"--env", "HOME=/tmp",
		"--env", "CGO_ENABLED=0",
	}
	// Go 专用的缓存和离线开关只对 Go 项目注入，其他语言不需要也不该看到 GOPATH 之类的变量
	if s.Kind == KindGo {
		goOffline := s.Network == "none"
		args = append(args,
			"--env", "GOCACHE=/tmp/gocache",
			"--env", "GOPATH=/tmp/gopath", // 根文件系统只读；go 会把校验库缓存写到 GOPATH 下
			"--env", "GOTMPDIR=/tmp",
			"--env", "GOTOOLCHAIN=local", // 不让 go 命令在运行时下载别的工具链
		)
		if goOffline {
			// 断网后任何联网的 go 命令都应该立刻失败，而不是卡到超时
			args = append(args, "--env", "GOFLAGS=-mod=readonly", "--env", "GOPROXY=off")
		}
	}
	if s.CacheDir != "" && s.CacheTarget != "" {
		mount := "type=bind,source=" + s.CacheDir + ",target=" + s.CacheTarget
		if !s.CacheWritable {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
		// 只挂载还不够：go 必须被明确告知模块缓存的位置，否则会去 GOPATH 下的默认路径找，
		// 断网时就只会报 GOPROXY=off
		if s.Kind == KindGo {
			args = append(args, "--env", "GOMODCACHE="+s.CacheTarget)
		}
	} else if s.Kind == KindGo {
		// 没有挂载缓存时仍然给一个可写位置（/tmp 是 tmpfs）
		args = append(args, "--env", "GOMODCACHE=/tmp/gomod")
	}
	if s.Network == "none" && s.Kind == KindPython {
		// 断网后不要尝试访问 PyPI，直接失败
		args = append(args, "--env", "PIP_NO_INDEX=1")
	}
	switch s.Kind {
	case KindNode:
		args = append(args,
			"--env", "npm_config_cache=/tmp/npm-cache",
			"--env", "npm_config_update_notifier=false",
			"--env", "CI=true",
		)
		if s.Network == "none" {
			args = append(args, "--env", "npm_config_offline=true")
		}
	case KindPython:
		args = append(args,
			"--env", "PYTHONUNBUFFERED=1",
			"--env", "PYTHONDONTWRITEBYTECODE=1", // 根文件系统只读，不能写 __pycache__
			"--env", "PYTHONPATH="+s.CacheTarget,
		)
	}
	for _, k := range sortedKeys(s.Env) {
		args = append(args, "--env", k+"="+s.Env[k])
	}
	return append(args, s.Image, "sh", "-c", s.Command)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单插入排序，避免为一个小函数引入 sort 的泛型写法
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Status 是一步检查的结论。
type Status string

const (
	Passed  Status = "passed"
	Failed  Status = "failed" // 命令正常运行完，但退出码非 0：测试或 Lint 没通过
	Errored Status = "error"  // 沙盒本身出问题：Docker 异常、超时、OOM
)

// Result 是一次容器运行的结果。
type Result struct {
	Name      string
	Status    Status
	ExitCode  int
	Duration  time.Duration
	OOM       bool
	// SuspectedOOM 表示容器被 SIGKILL 杀掉（退出码 137），但 Docker 没有给出 OOMKilled 标志。
	// 部分环境下 Docker 会随机丢失这个标志（内核日志里 OOM kill 实际发生了），
	// 所以不能只信标志：这种情况按资源问题处理，而不是当成测试失败。
	SuspectedOOM bool
	TimedOut  bool
	Output    string // 末尾的输出，已截断
	Truncated bool
	// Err 是沙盒自身的错误（Docker 起不来、镜像拉取失败等），不是命令失败。
	Err error
}

// Detail 返回一句话说明，用于总评。
func (r Result) Detail() string {
	switch {
	case r.Err != nil:
		return "沙盒错误：" + r.Err.Error()
	case r.OOM:
		return "内存溢出被终止（超过内存限制）"
	case r.SuspectedOOM:
		return "被 SIGKILL 终止（退出码 137），疑似内存溢出（超过内存限制）"
	case r.TimedOut:
		return "执行超时被终止"
	case r.Status == Failed:
		return "退出码 " + strconv.Itoa(r.ExitCode)
	default:
		return "通过"
	}
}

// Classify 把容器运行的原始信息归类为 Result.Status。
// 超时、OOM 和 Docker 自身的错误是 Errored：它们说明环境或资源有问题，
// 不能简单当作“测试没通过”。
func Classify(r Result) Status {
	switch {
	case r.Err != nil, r.OOM, r.SuspectedOOM, r.TimedOut:
		return Errored
	case r.ExitCode == 0:
		return Passed
	default:
		return Failed
	}
}

// Overall 汇总多个步骤的结论：只要有 Errored 就是 Errored，其次是 Failed，否则 Passed。
// 没有任何步骤时返回 Errored，因为这说明没有配置可执行的检查。
func Overall(results []Result) Status {
	if len(results) == 0 {
		return Errored
	}
	overall := Passed
	for _, r := range results {
		switch r.Status {
		case Errored:
			return Errored
		case Failed:
			overall = Failed
		}
	}
	return overall
}

// Describe 返回 commit status 的 state 和描述。描述是纯英文，
// 因为 GitHub 的 status 描述不接受 4 字节 Unicode。
func Describe(results []Result) (state, desc string) {
	switch Overall(results) {
	case Passed:
		return "success", "All sandbox checks passed"
	case Failed:
		return "failure", "Sandbox checks failed"
	default:
		return "error", "Sandbox execution error"
	}
}

func (s Spec) String() string {
	return fmt.Sprintf("%s [%s] net=%s mem=%s cpus=%s", s.Name, s.Image, s.Network, s.Memory, s.CPUs)
}
