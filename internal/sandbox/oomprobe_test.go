//go:build integration

package sandbox

import (
	"os/exec"
	"testing"
	"time"
)

// 取证用：打印 OOM 容器在 inspect 里的真实状态
func TestProbeOOM(t *testing.T) {
	name := "prr-probe-" + randomHex(4)
	cmd := exec.Command("docker", "run", "--name", name, "--memory", "64m", "--memory-swap", "64m",
		"--user", "65534:65534", "--read-only", "--network", "none", "alpine:3.20",
		"sh", "-c", `awk 'BEGIN{s="x"; while(1) s = s s}'`)
	out, err := cmd.CombinedOutput()
	t.Logf("run err=%v out=%q", err, string(out))
	time.Sleep(500 * time.Millisecond)
	for _, f := range []string{"{{.State.OOMKilled}}", "{{.State.ExitCode}}", "{{.State.Status}}", "{{.State.Error}}"} {
		o, e := exec.Command("docker", "inspect", "--format", f, name).CombinedOutput()
		t.Logf("inspect %s => %q err=%v", f, string(o), e)
	}
	info, _ := exec.Command("docker", "info", "--format", "cgroup={{.CgroupVersion}} driver={{.CgroupDriver}} swap={{.SwapLimit}} mem={{.MemoryLimit}}").CombinedOutput()
	t.Logf("docker info: %s", info)
	exec.Command("docker", "rm", "-f", name).Run()
}
