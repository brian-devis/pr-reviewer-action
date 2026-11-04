# PR AI Reviewer

用大模型审查 Pull Request 的 GitHub Action：拉取 PR 的 diff，调用你指定的模型接口，把问题以**行内评论**发到对应代码行，并在讨论区维护一条总评。

- 模型厂商、模型名、接口地址都可配置，支持任何 OpenAI Chat Completions 兼容的服务（OpenAI、DeepSeek、各类网关和本地模型服务）
- 无需服务器：作为 Action 在 GitHub Runner 上运行，下载预编译的单个二进制
- 大 PR 自动分批；自动忽略 lockfile、`vendor/`、生成文件等
- 再次 push 不会刷屏：总评原地更新，已评论过的位置不会重复发

## 使用

```yaml
name: AI PR Review
on:
  pull_request:
    types: [opened, synchronize, reopened]

permissions:
  contents: read
  pull-requests: write

jobs:
  review:
    # fork 的 PR 拿不到 secrets，跳过
    if: github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest
    steps:
      - uses: brian-devis/pr-reviewer-action@v1
        with:
          api_key: ${{ secrets.LLM_API_KEY }}
          api_base_url: https://api.deepseek.com
          model: deepseek-chat
```

不需要 `actions/checkout`：审查只读取 PR 的 diff。

## 输入

| 名称 | 默认值 | 说明 |
|---|---|---|
| `api_key` | 必填 | 模型服务的 API Key，请放在 Secrets 里 |
| `api_base_url` | `https://api.openai.com/v1` | 服务地址，会在其后拼接 `/chat/completions` |
| `model` | `gpt-4.1` | 模型标识 |
| `api_format` | `openai` | 目前仅支持 `openai` |
| `github_token` | `${{ github.token }}` | 需要 `pull-requests: write` |
| `max_diff_bytes` | `60000` | 单次模型请求携带的 diff 上限，超出会分批；单个文件超出会被截断 |
| `max_requests` | `5` | 单个 PR 最多发起的模型请求数，超出的文件不审查，并在总评里点名 |
| `exclude_paths` | 空 | 额外排除的路径，逗号或换行分隔；`dir/` 表示目录，`*.md` 之类按 `path.Match` 匹配（不支持 `**`） |

## 版本与分发

- `@v1`：跟随 v1 系列的最新发布；`@v1.2.3`：固定版本。两者都会下载预编译二进制，并用 Release 里的 `checksums.txt` 校验，失败时自动退回从源码构建。
- 引用分支（如 `@main`）会从源码构建，需要 runner 能下载 Go 工具链。
- 预编译二进制只提供 Linux（amd64 / arm64）。校验和与二进制来自同一个 Release，能发现下载损坏，但不能防御 Release 本身被篡改。

## 安全说明

- **fork PR**：`pull_request` 事件下 fork 没有 secrets，上面的示例直接跳过。不要为了让 fork 能用而改成 `pull_request_target` 再检出 fork 代码。
- **prompt 注入**：diff 被包在每次随机生成的标签里，并要求模型忽略其中的指令；模型输出只用来写评论，不会触发任何其他动作。模型输出里的 `@用户`、伪造的隐藏标记会被清理。
- **API Key**：运行时会用 `::add-mask::` 脱敏，不会写入日志。
- **成本**：diff 会发送给你配置的模型服务。请用 `exclude_paths` 排除不想外发的文件，用 `max_requests` 控制单个 PR 的调用次数。

## 开发

```bash
go vet ./...
go test ./...
```

目录：`cmd/reviewer` 是入口；`internal/` 下依次是 `config`（环境变量与事件）、`diff`（解析、过滤、分块）、`llm`（模型接口）、`review`（prompt、解析、渲染）、`ghclient`（GitHub API）、`httpx`（带重试的请求）。

发布：推送 `vX.Y.Z` tag，`release.yml` 会构建二进制、生成校验和、创建 Release，并把 `vX` 移动到该版本。
