package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind 是自动识别出的项目类型。
type Kind string

const (
	KindGo      Kind = "go"
	KindNode    Kind = "node"
	KindPython  Kind = "python"
	KindGeneric Kind = "generic"
)

// Project 是项目识别结果：它决定默认镜像、默认命令和依赖准备方式。
type Project struct {
	Kind  Kind
	Label string // 用于错误信息，如 "Node.js"
	// DefaultImage 是没有指定 sandbox_image 时使用的镜像。
	DefaultImage string
	// TestCmd / LintCmd 是项目类型对应的默认命令。
	TestCmd string
	LintCmd string
	// InstallCmd 是联网准备依赖的命令。它必须只下载依赖、不执行 PR 代码，
	// 由 --ignore-scripts（npm）和 --only-binary=:all:（pip）保证。
	// 为空表示这个项目不需要下载依赖。
	InstallCmd string
	// CacheTarget 是容器内存放依赖的目录，宿主机缓存目录挂载到这里。
	// 为空表示依赖直接装在工作副本 /work 里（如 node_modules）。
	CacheTarget string
}

var (
	nodeVersionRe   = regexp.MustCompile(`([0-9]+)`)
	pythonVersionRe = regexp.MustCompile(`([0-9]+)\.([0-9]+)`)
	requiresPyRe    = regexp.MustCompile(`requires-python\s*=\s*"[^"]*?([0-9]+)\.([0-9]+)`)
)

// Detect 根据仓库里的标志文件判断项目类型。一个仓库同时有多种类型时按
// go.mod > package.json > Python 的顺序取第一个，其他情况请在 workflow 里
// 显式指定 sandbox_image 和命令。
func Detect(dir string) Project {
	switch {
	case HasFile(dir, "go.mod"):
		v := GoVersion(dir)
		if v == "" {
			v = "1.23"
		}
		return Project{
			Kind:         KindGo,
			Label:        "Go",
			DefaultImage: "golang:" + v,
			TestCmd:      "go test ./...",
			LintCmd:      "go vet ./...",
			InstallCmd:   "go mod download",
			CacheTarget:  "/gomod",
		}
	case HasFile(dir, "package.json"):
		return Project{
			Kind:         KindNode,
			Label:        "Node.js",
			DefaultImage: "node:" + nodeVersion(dir),
			TestCmd:      "npm test --if-present",
			LintCmd:      "npm run lint --if-present",
			InstallCmd:   npmInstallCmd(dir),
		}
	case isPython(dir):
		return Project{
			Kind:         KindPython,
			Label:        "Python",
			DefaultImage: "python:" + pythonVersion(dir),
			TestCmd:      "python -m pytest -q",
			LintCmd:      "python -m compileall -q .",
			InstallCmd:   pipInstallCmd(dir),
			CacheTarget:  "/deps",
		}
	default:
		// 没有指定镜像时保持旧行为（golang 镜像），避免已有的非 Go 项目配置突然报错。
		return Project{Kind: KindGeneric, Label: "通用", DefaultImage: "golang:1.23"}
	}
}

// Detected 返回仓库里所有匹配的项目类型，顺序与 Detect 的优先级一致。
// 用来在同时存在多种标志（如 Go 后端加前端）时提示用户，而不是静默选一个。
func Detected(dir string) []Kind {
	var kinds []Kind
	if HasFile(dir, "go.mod") {
		kinds = append(kinds, KindGo)
	}
	if HasFile(dir, "package.json") {
		kinds = append(kinds, KindNode)
	}
	if isPython(dir) {
		kinds = append(kinds, KindPython)
	}
	return kinds
}

func isPython(dir string) bool {
	return HasFile(dir, "pyproject.toml") || HasFile(dir, "setup.py") ||
		HasFile(dir, "requirements.txt") || HasFile(dir, "requirements-dev.txt")
}

// npmInstallCmd 生成安装 Node 依赖的命令。--ignore-scripts 是关键：npm 默认会执行
// 包里的安装脚本，那等于执行 PR 依赖中的任意代码，即使只是“准备依赖”阶段也不行。
func npmInstallCmd(dir string) string {
	if HasFile(dir, "package-lock.json") || HasFile(dir, "npm-shrinkwrap.json") {
		return "npm ci --ignore-scripts --no-audit --no-fund"
	}
	return "npm install --ignore-scripts --no-audit --no-fund"
}

// pipInstallCmd 生成安装 requirements 的命令。--only-binary=:all: 拒绝源码包，
// 因为构建源码包会执行 setup.py，相当于执行 PR 里的代码。需要源码包的依赖
// 请在 workflow 里用 sandbox_install_command 显式配置。
func pipInstallCmd(dir string) string {
	var files []string
	for _, f := range []string{"requirements.txt", "requirements-dev.txt"} {
		if HasFile(dir, f) {
			files = append(files, "-r "+f)
		}
	}
	if len(files) == 0 {
		return ""
	}
	return "pip install --disable-pip-version-check --no-input --only-binary=:all: --target /deps " +
		strings.Join(files, " ")
}

// nodeVersion 从 .nvmrc、.node-version 或 package.json 的 engines.node 里读出
// 主版本号，读不到返回 22。
func nodeVersion(dir string) string {
	for _, f := range []string{".nvmrc", ".node-version"} {
		if data, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if v := firstNumber(string(data)); v != "" {
				return v
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var p struct {
			Engines struct {
				Node string `json:"node"`
			} `json:"engines"`
		}
		if json.Unmarshal(data, &p) == nil {
			if v := firstNumber(p.Engines.Node); v != "" {
				return v
			}
		}
	}
	return "22"
}

func firstNumber(s string) string {
	return nodeVersionRe.FindString(s)
}

// pythonVersion 从 .python-version 或 pyproject.toml 的 requires-python 里读出
// 主次版本号，读不到返回 3.12。
func pythonVersion(dir string) string {
	if data, err := os.ReadFile(filepath.Join(dir, ".python-version")); err == nil {
		if m := pythonVersionRe.FindStringSubmatch(string(data)); m != nil {
			return m[1] + "." + m[2]
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "pyproject.toml")); err == nil {
		if m := requiresPyRe.FindStringSubmatch(string(data)); m != nil {
			return m[1] + "." + m[2]
		}
	}
	return "3.12"
}
