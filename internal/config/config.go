// Package config 从 Actions 注入的环境变量和事件文件中读取运行配置。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/brian-devis/pr-reviewer-action/internal/review"
)

type Config struct {
	GitHubToken string
	Repo        string
	GitHubAPI   string
	HeadSHA     string // PR 的 head 提交，状态和评论都关联到它，而不是 pull_request 默认检出的合并提交
	PRNumber    int
	// RunURL 是本次 workflow 运行的页面，作为 commit status 的 target_url。
	RunURL string

	// 两个功能完全独立：关掉 AI 评审时，不要求也不会读取任何模型配置。
	EnableAI      bool
	EnableSandbox bool

	// AI 评审
	APIKey        string
	APIBaseURL    string
	Model         string
	APIFormat     string
	MaxDiffBytes  int // 单次模型请求携带的 diff 上限
	MaxRequests   int // 单个 PR 最多发起的模型请求数
	Exclude       []string
	FailOn        string // never | error | warning
	StatusContext string // AI 评审的 commit status 名称

	// 沙盒测试
	Workspace            string // 仓库检出目录（GITHUB_WORKSPACE）
	SandboxLanguage      string // auto | go | node | python
	SandboxImage         string
	SandboxTestCmd       string
	SandboxLintCmd       string
	SandboxInstallCmd    string
	SandboxMemory        string
	SandboxCPUs          string
	SandboxTimeout       time.Duration
	SandboxStatusContext string
}

// formatDefaults 是各 API 协议在用户没有填写 api_base_url / model 时的默认值。
var formatDefaults = map[string]struct{ baseURL, model string }{
	"openai":    {"https://api.openai.com/v1", "gpt-4.1"},
	"anthropic": {"https://api.anthropic.com", "claude-sonnet-5-5"},
}

var (
	// 内存限制：数字加可选的 b/k/m/g 单位，与 docker --memory 一致
	memoryRe = regexp.MustCompile(`^[1-9][0-9]*[bkmgBKMG]?$`)
	// 镜像名：不能以 - 开头，也不能含空格或其他会被 docker 当成选项或参数的字符
	imageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

const maxSandboxTimeout = time.Hour

func Load() (*Config, error) {
	c := &Config{
		GitHubToken: os.Getenv("INPUT_GITHUB_TOKEN"),
		Repo:        os.Getenv("GITHUB_REPOSITORY"),
		GitHubAPI:   strings.TrimRight(envDefault("GITHUB_API_URL", "https://api.github.com"), "/"),
		Workspace:   os.Getenv("GITHUB_WORKSPACE"),
	}
	if c.GitHubToken == "" || c.Repo == "" {
		return nil, errors.New("缺少 github_token 或 GITHUB_REPOSITORY")
	}
	if server, run := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_RUN_ID"); server != "" && run != "" {
		c.RunURL = fmt.Sprintf("%s/%s/actions/runs/%s", strings.TrimRight(server, "/"), c.Repo, run)
	}

	var err error
	if c.EnableAI, err = envBool("INPUT_ENABLE_AI_REVIEW", true); err != nil {
		return nil, err
	}
	if c.EnableSandbox, err = envBool("INPUT_ENABLE_SANDBOX", false); err != nil {
		return nil, err
	}
	if c.EnableAI {
		if err := c.loadAI(); err != nil {
			return nil, err
		}
	}
	if c.EnableSandbox {
		if err := c.loadSandbox(); err != nil {
			return nil, err
		}
	}
	if err := c.loadEvent(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadAI 读取并校验 AI 评审的配置。只有 enable_ai_review 打开时才会调用。
func (c *Config) loadAI() error {
	c.APIKey = os.Getenv("INPUT_API_KEY")
	c.APIBaseURL = strings.TrimRight(strings.TrimSpace(os.Getenv("INPUT_API_BASE_URL")), "/")
	c.Model = strings.TrimSpace(os.Getenv("INPUT_MODEL"))
	c.APIFormat = strings.ToLower(envDefault("INPUT_API_FORMAT", "openai"))
	c.Exclude = splitList(os.Getenv("INPUT_EXCLUDE_PATHS"))
	c.FailOn = strings.ToLower(envDefault("INPUT_FAIL_ON", "never"))
	c.StatusContext = envDefault("INPUT_STATUS_CONTEXT", "ai-review")

	if !review.ValidFailOn(c.FailOn) {
		return fmt.Errorf("fail_on 取值无效 %q，可选 never、error、warning", c.FailOn)
	}
	if c.APIKey == "" {
		return errors.New("启用 AI 评审时必须提供 api_key；只想运行沙盒测试请设置 enable_ai_review: 'false'")
	}
	d, ok := formatDefaults[c.APIFormat]
	if !ok {
		return fmt.Errorf("不支持的 api_format %q，目前支持 openai、anthropic", c.APIFormat)
	}
	if c.APIBaseURL == "" {
		c.APIBaseURL = d.baseURL
	}
	if c.Model == "" {
		c.Model = d.model
	}

	var err error
	if c.MaxDiffBytes, err = positiveInt("INPUT_MAX_DIFF_BYTES", "60000"); err != nil {
		return err
	}
	c.MaxRequests, err = positiveInt("INPUT_MAX_REQUESTS", "5")
	return err
}

// loadSandbox 读取并校验沙盒测试的配置。只有 enable_sandbox 打开时才会调用。
func (c *Config) loadSandbox() error {
	c.SandboxLanguage = strings.ToLower(envDefault("INPUT_SANDBOX_LANGUAGE", "auto"))
	c.SandboxImage = strings.TrimSpace(os.Getenv("INPUT_SANDBOX_IMAGE"))
	c.SandboxTestCmd = strings.TrimSpace(os.Getenv("INPUT_SANDBOX_TEST_COMMAND"))
	c.SandboxLintCmd = strings.TrimSpace(os.Getenv("INPUT_SANDBOX_LINT_COMMAND"))
	c.SandboxInstallCmd = strings.TrimSpace(os.Getenv("INPUT_SANDBOX_INSTALL_COMMAND"))
	c.SandboxMemory = strings.ToLower(envDefault("INPUT_SANDBOX_MEMORY", "1g"))
	c.SandboxStatusContext = envDefault("INPUT_SANDBOX_STATUS_CONTEXT", "sandbox-tests")

	if c.Workspace == "" {
		return errors.New("缺少 GITHUB_WORKSPACE：沙盒需要代码，请先使用 actions/checkout")
	}
	switch c.SandboxLanguage {
	case "auto", "go", "node", "python":
	default:
		return fmt.Errorf("sandbox_language 取值无效 %q，可选 auto、go、node、python", c.SandboxLanguage)
	}
	if c.SandboxImage != "" && !imageRe.MatchString(c.SandboxImage) {
		return fmt.Errorf("sandbox_image 取值无效 %q", c.SandboxImage)
	}
	if !memoryRe.MatchString(c.SandboxMemory) {
		return fmt.Errorf("sandbox_memory 取值无效 %q，示例：512m、1g", c.SandboxMemory)
	}
	// 沙盒只允许断网运行。这个开关保留是为了让配置显式、也防止有人误以为可以联网。
	if n := strings.ToLower(envDefault("INPUT_SANDBOX_NETWORK", "none")); n != "none" {
		return fmt.Errorf("sandbox_network 只支持 none，不允许为 PR 代码开放网络，got %q", n)
	}

	cpus, err := strconv.ParseFloat(envDefault("INPUT_SANDBOX_CPUS", "1"), 64)
	if err != nil || cpus <= 0 || cpus > 64 {
		return errors.New("sandbox_cpus 必须是 0 到 64 之间的正数")
	}
	c.SandboxCPUs = strconv.FormatFloat(cpus, 'f', -1, 64)

	c.SandboxTimeout, err = time.ParseDuration(envDefault("INPUT_SANDBOX_TIMEOUT", "5m"))
	if err != nil || c.SandboxTimeout <= 0 || c.SandboxTimeout > maxSandboxTimeout {
		return fmt.Errorf("sandbox_timeout 必须是大于 0 且不超过 %s 的时长，示例：90s、5m", maxSandboxTimeout)
	}
	return nil
}

// loadEvent 从事件文件里读出 PR 编号和 head 提交。
func (c *Config) loadEvent() error {
	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return fmt.Errorf("读取事件文件失败: %w", err)
	}
	var ev struct {
		Number      int `json:"number"`
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("解析事件文件失败: %w", err)
	}
	c.PRNumber = ev.PullRequest.Number
	if c.PRNumber == 0 {
		c.PRNumber = ev.Number
	}
	if c.PRNumber == 0 {
		return errors.New("当前事件不是 pull_request 事件")
	}
	c.HeadSHA = ev.PullRequest.Head.SHA
	return nil
}

func envDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envBool 读取布尔环境变量，空值返回默认值，无法识别的值报错而不是悄悄当成 false。
func envBool(key string, def bool) (bool, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return def, nil
	}
	switch v {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("%s 必须是 true 或 false，got %q", strings.ToLower(strings.TrimPrefix(key, "INPUT_")), v)
}

func positiveInt(key, def string) (int, error) {
	n, err := strconv.Atoi(envDefault(key, def))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s 必须是正整数", strings.ToLower(strings.TrimPrefix(key, "INPUT_")))
	}
	return n, nil
}

// splitList 按换行或逗号拆分，去掉空白和空项。
func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
