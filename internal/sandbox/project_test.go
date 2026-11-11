package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nodeProject(t *testing.T) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{"name":"demo","engines":{"node":">=20.9.0"}}`)
	write(t, filepath.Join(dir, "index.js"), "console.log(1)")
	return dir
}

func pythonProject(t *testing.T) string {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"demo\"\nrequires-python = \">=3.10\"\n")
	write(t, filepath.Join(dir, "requirements.txt"), "pytest==8.3.4\n")
	write(t, filepath.Join(dir, "app.py"), "print(1)")
	return dir
}

func TestDetect(t *testing.T) {
	t.Run("Go", func(t *testing.T) {
		p := Detect(goProject(t))
		if p.Kind != KindGo || p.DefaultImage != "golang:1.22" || p.InstallCmd != "go mod download" || p.CacheTarget != "/gomod" {
			t.Fatalf("%+v", p)
		}
	})
	t.Run("Node", func(t *testing.T) {
		p := Detect(nodeProject(t))
		if p.Kind != KindNode || p.DefaultImage != "node:20" {
			t.Fatalf("应从 engines.node 读出主版本: %+v", p)
		}
		if !strings.Contains(p.InstallCmd, "--ignore-scripts") {
			t.Fatalf("安装依赖不能执行 PR 的安装脚本: %q", p.InstallCmd)
		}
		if p.TestCmd != "npm test --if-present" || p.LintCmd != "npm run lint --if-present" {
			t.Fatalf("默认命令不对: %+v", p)
		}
	})
	t.Run("Node 用 lockfile 时走 npm ci", func(t *testing.T) {
		dir := nodeProject(t)
		write(t, filepath.Join(dir, "package-lock.json"), "{}")
		p := Detect(dir)
		if !strings.HasPrefix(p.InstallCmd, "npm ci ") || !strings.Contains(p.InstallCmd, "--ignore-scripts") {
			t.Fatalf("%q", p.InstallCmd)
		}
	})
	t.Run("Node 版本来自 .nvmrc", func(t *testing.T) {
		dir := nodeProject(t)
		write(t, filepath.Join(dir, ".nvmrc"), "v18.19.0\n")
		if p := Detect(dir); p.DefaultImage != "node:18" {
			t.Fatalf("%q", p.DefaultImage)
		}
	})
	t.Run("Python", func(t *testing.T) {
		p := Detect(pythonProject(t))
		if p.Kind != KindPython || p.DefaultImage != "python:3.10" || p.CacheTarget != "/deps" {
			t.Fatalf("%+v", p)
		}
		if !strings.Contains(p.InstallCmd, "--only-binary=:all:") || !strings.Contains(p.InstallCmd, "--target /deps") {
			t.Fatalf("安装依赖不能构建源码包，且要装到 /deps: %q", p.InstallCmd)
		}
		if !strings.Contains(p.InstallCmd, "-r requirements.txt") {
			t.Fatalf("%q", p.InstallCmd)
		}
	})
	t.Run("Python 无 requirements 时不安装", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "setup.py"), "print(1)")
		if p := Detect(dir); p.InstallCmd != "" {
			t.Fatalf("没有 requirements 时不应联网: %q", p.InstallCmd)
		}
	})
	t.Run("通用项目", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "Makefile"), "test:\n\techo ok")
		p := Detect(dir)
		if p.Kind != KindGeneric || p.TestCmd != "" || p.InstallCmd != "" {
			t.Fatalf("%+v", p)
		}
	})
}

func TestResolveNodeAndPython(t *testing.T) {
	o, err := Resolve(Options{Source: nodeProject(t)})
	if err != nil {
		t.Fatal(err)
	}
	if o.Image != "node:20" || !strings.Contains(o.InstallCmd, "npm") {
		t.Fatalf("%+v", o)
	}

	o, err = Resolve(Options{Source: pythonProject(t)})
	if err != nil {
		t.Fatal(err)
	}
	if o.Image != "python:3.10" || o.CacheTarget != "/deps" || o.TestCmd != "python -m pytest -q" {
		t.Fatalf("%+v", o)
	}

	// 显式指定类型和命令时不覆盖用户的配置，也不塞默认 lint
	o, err = Resolve(Options{Source: t.TempDir(), Project: KindNode, TestCmd: "node test.js"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Image != "node:22" || o.TestCmd != "node test.js" || o.LintCmd != "" {
		t.Fatalf("%+v", o)
	}
}

func TestRunNodeAndPythonOrchestration(t *testing.T) {
	tests := []struct {
		name     string
		src      func(*testing.T) string
		wantCmd  string
		wantKind Kind
	}{
		{"Node", nodeProject, "npm ", KindNode},
		{"Python", pythonProject, "pip install", KindPython},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{}
			o, err := Resolve(Options{Source: tt.src(t), Memory: "1g", CPUs: "1", Timeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Run(context.Background(), f, o); err != nil {
				t.Fatal(err)
			}
			if len(f.specs) != 3 {
				t.Fatalf("应有 deps、test、lint 三次运行，got %d", len(f.specs))
			}
			deps, test, lint := f.specs[0], f.specs[1], f.specs[2]
			if !strings.Contains(deps.Command, tt.wantCmd) {
				t.Errorf("准备命令不对: %q", deps.Command)
			}
			if deps.Network != "bridge" || !deps.CacheWritable {
				t.Errorf("准备阶段应联网且缓存可写: %+v", deps)
			}
			for _, s := range []Spec{test, lint} {
				if s.Network != "none" || s.CacheWritable {
					t.Errorf("运行阶段必须断网、缓存只读: %+v", s)
				}
				if s.Kind != tt.wantKind {
					t.Errorf("项目类型应传递到 Spec: %s", s.Kind)
				}
			}
		})
	}
}

func TestResolveReportsOtherDetectedKinds(t *testing.T) {
	// Go 后端加前端：识别按优先级选 Go，但必须把 Node 也报告出来，不能静默选择
	dir := goProject(t)
	write(t, filepath.Join(dir, "package.json"), `{"name":"web"}`)

	o, err := Resolve(Options{Source: dir})
	if err != nil {
		t.Fatal(err)
	}
	if o.Project != KindGo || len(o.AlsoDetected) != 1 || o.AlsoDetected[0] != KindNode {
		t.Fatalf("应选 Go 并报告 Node: project=%s also=%v", o.Project, o.AlsoDetected)
	}

	// 显式指定类型：用户已经做了选择，不再提示
	o, _ = Resolve(Options{Source: dir, Project: KindNode})
	if len(o.AlsoDetected) != 0 {
		t.Fatalf("显式指定 sandbox_language 时不应提示: %v", o.AlsoDetected)
	}
	o, _ = Resolve(Options{Source: dir, Project: "auto"})
	if o.Project != KindGo || len(o.AlsoDetected) != 1 {
		t.Fatalf("auto 等同于自动识别: %+v", o)
	}

	// 镜像和命令都写死了，识别结果没有决定任何东西，提示只是噪音
	o, _ = Resolve(Options{Source: dir, Image: "golang:1.25", TestCmd: "make test"})
	if len(o.AlsoDetected) != 0 {
		t.Fatalf("配置已写死时不应提示: %v", o.AlsoDetected)
	}

	// 只有一种类型时没有可提示的
	o, _ = Resolve(Options{Source: goProject(t)})
	if len(o.AlsoDetected) != 0 {
		t.Fatalf("%v", o.AlsoDetected)
	}
}

func TestDetectedKeepsPriorityOrder(t *testing.T) {
	dir := pythonProject(t)
	write(t, filepath.Join(dir, "package.json"), `{}`)
	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22\n")
	got := Detected(dir)
	if len(got) != 3 || got[0] != KindGo || got[1] != KindNode || got[2] != KindPython {
		t.Fatalf("%v", got)
	}
}
