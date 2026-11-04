# PR AI Reviewer

用大模型审查 Pull Request 的 GitHub Action：拉取 PR 的 diff，调用你指定的模型接口，把问题以**行内评论**发到对应代码行，并在讨论区维护一条总评。

- 模型厂商、模型名、接口地址都可配置：支持 OpenAI Chat Completions 兼容的服务（OpenAI、DeepSeek、各类网关和本地模型服务），以及 Anthropic 原生 Messages API
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
  statuses: write

jobs:
  review:
    # fork 的 PR 拿不到 secrets，跳过
    if: github.event.pull_request.head.repo.full_name == github.repository
    runs-on: ubuntu-latest
    steps:
      - uses: brian-devis/pr-reviewer-action@v0
        with:
          api_key: ${{ secrets.LLM_API_KEY }}
          api_base_url: https://api.deepseek.com
          model: deepseek-chat
```

不需要 `actions/checkout`：审查只读取 PR 的 diff。

使用 Anthropic 时只需换协议，地址和模型都有默认值：

```yaml
      - uses: brian-devis/pr-reviewer-action@v0
        with:
          api_format: anthropic
          api_key: ${{ secrets.ANTHROPIC_API_KEY }}
```

使用第三方中转站时，请把 `api_base_url` 设为中转站地址，并把 `model` 明确填成该中转站支持的模型名：默认的 `claude-sonnet-5-5` 是官方标识，中转站不一定放行（有的只认 `claude-sonnet-5` 这类名字）。

## 输入

| 名称 | 默认值 | 说明 |
|---|---|---|
| `api_key` | 必填 | 模型服务的 API Key，请放在 Secrets 里 |
| `api_format` | `openai` | 协议格式：`openai`（Chat Completions 兼容）或 `anthropic`（原生 Messages API） |
| `api_base_url` | 按协议 | 服务地址。留空时 `openai` 用 `https://api.openai.com/v1`，`anthropic` 用 `https://api.anthropic.com`。`openai` 会拼接 `/chat/completions`，`anthropic` 会拼接 `/v1/messages`（地址已以 `/v1` 结尾则不重复） |
| `model` | 按协议 | 模型标识。留空时 `openai` 用 `gpt-4.1`，`anthropic` 用 `claude-sonnet-5-5` |
| `github_token` | `${{ github.token }}` | 需要 `pull-requests: write` |
| `max_diff_bytes` | `60000` | 单次模型请求携带的 diff 上限，超出会分批；单个文件超出会被截断 |
| `max_requests` | `5` | 单个 PR 最多发起的模型请求数，超出的文件不审查，并在总评里点名 |
| `fail_on` | `never` | 达到该严重度时把 commit status 设为失败：`never` 只提示；`error` 有 🔴 就失败；`warning` 有 🔴 或 🟡 就失败 |
| `status_context` | `ai-review` | commit status 的名称，设为必需检查时在分支保护里选它 |
| `exclude_paths` | 空 | 额外排除的路径，逗号或换行分隔；`dir/` 表示目录，`*.md` 之类按 `path.Match` 匹配（不支持 `**`） |

## 作为合并门禁

审查结束后会给 PR 的最新提交设置一个名为 `ai-review` 的 commit status：运行中是 `pending`，完成后按 `fail_on` 判定为 `success` 或 `failure`，模型请求全部失败时是 `error`。需要在 workflow 里授予 `statuses: write`，没有权限时只会打印警告，不影响审查本身。

- 统计的是**本次模型发现的全部问题**，包括因为之前已经评论过而没有重复发布的，所以状态反映的是“当前代码还有没有问题”。
- 启用了 `fail_on` 但审查不完整（有模型请求失败、返回无法解析、或有文件因 `max_requests` 被跳过）时，状态是 `error`，不会在没审完的情况下显示通过。
- 步骤本身的退出码不受 `fail_on` 影响，是否阻断合并由状态检查和分支保护决定。
- 模型的判断有随机性，同一份代码两次运行可能结果不同。建议先用 `fail_on: never` 观察一段时间，再决定是否设为必需检查。

## 版本与分发

- 目前处于 0.x 阶段，输入参数仍可能调整。`@v0` 跟随 0.x 系列的最新发布；`@v0.2.1` 固定到具体版本，需要稳定行为时建议用这种。接口稳定后会发布 `v1.0.0`，届时可以用 `@v1`。
- 引用 `@v0` 或 `@vX.Y.Z` 时都会下载预编译二进制，并用 Release 里的 `checksums.txt` 校验，失败时自动退回从源码构建。早于 v0.2.1 的版本没有校验和文件，会直接从源码构建。
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
