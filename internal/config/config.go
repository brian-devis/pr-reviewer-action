// Package config 从 Actions 注入的环境变量和事件文件中读取运行配置。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	GitHubToken  string
	APIKey       string
	APIBaseURL   string
	Model        string
	APIFormat    string
	Repo         string
	GitHubAPI    string
	HeadSHA      string
	PRNumber     int
	MaxDiffBytes int // 单次模型请求携带的 diff 上限
	MaxRequests  int // 单个 PR 最多发起的模型请求数
	Exclude      []string
}

func Load() (*Config, error) {
	c := &Config{
		GitHubToken: os.Getenv("INPUT_GITHUB_TOKEN"),
		APIKey:      os.Getenv("INPUT_API_KEY"),
		APIBaseURL:  strings.TrimRight(envDefault("INPUT_API_BASE_URL", "https://api.openai.com/v1"), "/"),
		Model:       envDefault("INPUT_MODEL", "gpt-4.1"),
		APIFormat:   envDefault("INPUT_API_FORMAT", "openai"),
		Repo:        os.Getenv("GITHUB_REPOSITORY"),
		GitHubAPI:   strings.TrimRight(envDefault("GITHUB_API_URL", "https://api.github.com"), "/"),
		Exclude:     splitList(os.Getenv("INPUT_EXCLUDE_PATHS")),
	}
	if c.GitHubToken == "" || c.APIKey == "" || c.Repo == "" {
		return nil, errors.New("缺少 github_token、api_key 或 GITHUB_REPOSITORY")
	}
	if c.APIFormat != "openai" {
		return nil, fmt.Errorf("不支持的 api_format %q，目前仅支持 openai", c.APIFormat)
	}

	var err error
	if c.MaxDiffBytes, err = positiveInt("INPUT_MAX_DIFF_BYTES", "60000"); err != nil {
		return nil, err
	}
	if c.MaxRequests, err = positiveInt("INPUT_MAX_REQUESTS", "5"); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return nil, fmt.Errorf("读取事件文件失败: %w", err)
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
		return nil, fmt.Errorf("解析事件文件失败: %w", err)
	}
	c.PRNumber = ev.PullRequest.Number
	if c.PRNumber == 0 {
		c.PRNumber = ev.Number
	}
	if c.PRNumber == 0 {
		return nil, errors.New("当前事件不是 pull_request 事件")
	}
	c.HeadSHA = ev.PullRequest.Head.SHA
	return c, nil
}

func envDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
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
