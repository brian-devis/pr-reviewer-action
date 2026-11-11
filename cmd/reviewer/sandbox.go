package main

import (
	"context"
	"errors"

	"github.com/brian-devis/pr-reviewer-action/internal/config"
	"github.com/brian-devis/pr-reviewer-action/internal/review"
	"github.com/brian-devis/pr-reviewer-action/internal/sandbox"
)

// availabilityChecker 由能检查自身是否可用的 Runner（真实的 Docker）实现。
type availabilityChecker interface {
	Available(ctx context.Context) error
}

// runSandbox 执行沙盒检查，返回总评里的一节、commit status 的状态和描述。
//
// 只有沙盒本身无法运行（Docker 不可用、没有可执行的命令、复制工作副本失败）时才返回
// error；测试或 Lint 没通过只体现在状态里，不是错误。这个函数不接触 GitHub Token 和
// 任何模型配置，沙盒不依赖 AI 评审。
func runSandbox(ctx context.Context, cfg *config.Config, r sandbox.Runner) (review.Sandbox, string, string, error) {
	section := review.Sandbox{
		Image: cfg.SandboxImage, Memory: cfg.SandboxMemory, CPUs: cfg.SandboxCPUs, Network: "none",
	}
	fail := func(err error) (review.Sandbox, string, string, error) {
		section.Steps = []review.SandboxStep{{Name: "sandbox", Status: review.StepError, Detail: err.Error()}}
		return section, review.StateError, "Sandbox execution error", err
	}

	if c, ok := r.(availabilityChecker); ok {
		if err := c.Available(ctx); err != nil {
			return fail(err)
		}
	}
	opts, err := sandbox.Resolve(sandbox.Options{
		Image:      cfg.SandboxImage,
		TestCmd:    cfg.SandboxTestCmd,
		LintCmd:    cfg.SandboxLintCmd,
		InstallCmd: cfg.SandboxInstallCmd,
		Memory:     cfg.SandboxMemory,
		CPUs:       cfg.SandboxCPUs,
		Timeout:    cfg.SandboxTimeout,
		Source:     cfg.Workspace,
		Project:    sandbox.Kind(cfg.SandboxLanguage),
	})
	if err != nil {
		return fail(err)
	}
	section.Image = opts.Image
	section.Kind = string(opts.Project)
	for _, k := range opts.AlsoDetected {
		section.Also = append(section.Also, string(k))
	}

	rep, err := sandbox.Run(ctx, r, opts)
	if err != nil {
		return fail(err)
	}
	if len(rep.Results) == 0 {
		return fail(errors.New("没有执行任何检查"))
	}
	state, desc := sandbox.Describe(rep.Results)
	return toSection(rep), state, desc, nil
}

// toSection 把沙盒的运行结果转换成总评里展示的内容。
func toSection(rep sandbox.Report) review.Sandbox {
	s := review.Sandbox{
		Image: rep.Image, Memory: rep.Memory, CPUs: rep.CPUs, Network: rep.Network, Elapsed: rep.Elapsed,
	}
	for _, r := range rep.Results {
		s.Steps = append(s.Steps, review.SandboxStep{
			Name:      r.Name,
			Status:    string(r.Status),
			Detail:    r.Detail(),
			Duration:  r.Duration,
			Output:    r.Output,
			Truncated: r.Truncated,
		})
	}
	return s
}
