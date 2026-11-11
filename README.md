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
| `api_key` | 启用 AI 评审时必填 | 模型服务的 API Key，请放在 Secrets 里；只运行沙盒时不需要 |
| `api_format` | `openai` | 协议格式：`openai`（Chat Completions 兼容）或 `anthropic`（原生 Messages API） |
| `api_base_url` | 按协议 | 服务地址。留空时 `openai` 用 `https://api.openai.com/v1`，`anthropic` 用 `https://api.anthropic.com`。`openai` 会拼接 `/chat/completions`，`anthropic` 会拼接 `/v1/messages`（地址已以 `/v1` 结尾则不重复） |
| `model` | 按协议 | 模型标识。留空时 `openai` 用 `gpt-4.1`，`anthropic` 用 `claude-sonnet-5-5` |
| `github_token` | `${{ github.token }}` | 需要 `pull-requests: write` |
| `max_diff_bytes` | `60000` | 单次模型请求携带的 diff 上限，超出会分批；单个文件超出会被截断 |
| `max_requests` | `5` | 单个 PR 最多发起的模型请求数，超出的文件不审查，并在总评里点名 |
| `fail_on` | `never` | 达到该严重度时把 commit status 设为失败：`never` 只提示；`error` 有 🔴 就失败；`warning` 有 🔴 或 🟡 就失败 |
| `status_context` | `ai-review` | commit status 的名称，设为必需检查时在分支保护里选它 |
| `enable_ai_review` | `true` | 是否启用 AI 评审。设为 `false` 时不会初始化模型，也不要求任何模型配置 |
| `enable_sandbox` | `false` | 是否启用沙盒测试，见下文“沙盒测试” |
| `sandbox_language` | `auto` | 项目类型：`auto`（自动识别）、`go`、`node`、`python` |
| `sandbox_image` | 按项目 | 沙盒镜像，如 `golang:1.25`、`node:22`、`python:3.12`；不填时按项目类型和版本文件选择 |
| `sandbox_test_command` | 按项目 | 沙盒里的测试命令 |
| `sandbox_lint_command` | 按项目 | 沙盒里的 Lint 命令 |
| `sandbox_install_command` | 按项目 | 联网准备依赖的命令，只在准备阶段执行、不执行 PR 代码 |
| `sandbox_memory` | `1g` | 每个容器的内存上限 |
| `sandbox_cpus` | `1` | 每个容器的 CPU 上限 |
| `sandbox_timeout` | `5m` | 每个步骤的超时，最长 `1h` |
| `sandbox_network` | `none` | 目前只支持 `none` |
| `sandbox_status_context` | `sandbox-tests` | 沙盒 commit status 的名称；同一仓库有多个沙盒 job 时各自设成不同的值，评论和状态才不会互相覆盖 |
| `exclude_paths` | 空 | 额外排除的路径，逗号或换行分隔；`dir/` 表示目录，`*.md` 之类按 `path.Match` 匹配（不支持 `**`） |

## 作为合并门禁

审查结束后会给 PR 的最新提交设置一个名为 `ai-review` 的 commit status：运行中是 `pending`，完成后按 `fail_on` 判定为 `success` 或 `failure`，模型请求全部失败时是 `error`。需要在 workflow 里授予 `statuses: write`，没有权限时只会打印警告，不影响审查本身。

- 统计的是**本次模型发现的全部问题**，包括因为之前已经评论过而没有重复发布的，所以状态反映的是“当前代码还有没有问题”。
- 启用了 `fail_on` 但审查不完整（有模型请求失败、返回无法解析、或有文件因 `max_requests` 被跳过）时，状态是 `error`，不会在没审完的情况下显示通过。
- 步骤本身的退出码不受 `fail_on` 影响，是否阻断合并由状态检查和分支保护决定。
- 模型的判断有随机性，同一份代码两次运行可能结果不同。建议先用 `fail_on: never` 观察一段时间，再决定是否设为必需检查。

## 沙盒测试

在断网、限制资源的 Docker 容器里运行测试和 Lint，把结果写进 PR 总评和独立的 `sandbox-tests` commit status。**它独立于 AI 评审**：不配置任何模型也能运行，反过来也一样。

| `enable_ai_review` | `enable_sandbox` | 行为 |
|---|---|---|
| `true`（默认） | `false`（默认） | 只做 AI 评审 |
| `true` | `true` | AI 评审 + 沙盒检查，结果合并在同一条总评里 |
| `false` | `true` | 只运行测试和 Lint，不要求 `api_key`、`api_base_url`、`model` |
| `false` | `false` | 什么都不做，不访问任何外部服务 |

只开沙盒的最小示例（沙盒需要代码，所以要先 checkout）：

```yaml
permissions:
  contents: read
  pull-requests: write
  statuses: write

jobs:
  sandbox:
    runs-on: ubuntu-latest   # 只支持 Linux runner
    steps:
      - uses: actions/checkout@v4
        with:
          persist-credentials: false
          ref: ${{ github.event.pull_request.head.sha }}
      - uses: brian-devis/pr-reviewer-action@v0
        with:
          enable_ai_review: "false"
          enable_sandbox: "true"
          sandbox_test_command: go test ./...
          sandbox_lint_command: go vet ./...
```

### 支持的语言

| 类型 | 识别标志 | 默认镜像 | 默认 test | 默认 lint | 准备依赖 |
|---|---|---|---|---|---|
| Go | `go.mod` | `golang:<go.mod 的版本>` | `go test ./...` | `go vet ./...` | `go mod download` |
| Node.js | `package.json` | `node:<.nvmrc / .node-version / engines.node，默认 22>` | `npm test --if-present` | `npm run lint --if-present` | `npm ci`（有 lockfile）或 `npm install`，都带 `--ignore-scripts` |
| Python | `pyproject.toml`、`setup.py`、`requirements.txt` | `python:<.python-version / requires-python，默认 3.12>` | `python -m pytest -q` | `python -m compileall -q .` | `pip install --only-binary=:all: --target /deps -r requirements.txt` |
| 其他 | — | 需自己指定 | 需自己指定 | 需自己指定 | 不联网 |

同时存在多种标志时按 Go → Node → Python 的顺序取第一个（例如 Go 后端加前端的仓库会被识别为 Go）。这时总评里会注明“自动识别为 Go，同时还检测到 Node.js 的标志文件”，识别错了就用 `sandbox_language` 指定类型，或者直接写死镜像和命令。显式指定了 `sandbox_language`，或者镜像和命令都已写死时不会出现这条提示。

`npm test` / `npm run lint` 都带 `--if-present`：项目没定义对应脚本时不算失败，所以大多数没有 `lint` 脚本的 Node 项目仍能正常通过。

### 怎么运行

1. **准备依赖（联网，不执行 PR 代码）**：在受限容器里下载依赖。这一步是唯一联网的，且被刻意设计成不执行 PR 里的代码：
   - Go：`go mod download` 只下载模块。
   - Node：`--ignore-scripts` 禁止 npm 运行包里的 `postinstall` 等脚本。
   - Python：`--only-binary=:all:` 拒绝源码包，因为构建源码包会执行 `setup.py`。
   - 需要源码包或自定义安装步骤时，用 `sandbox_install_command` 显式配置，并清楚它会执行 PR 里的代码。
2. **运行测试**：独立容器，断网，依赖只读挂载（Go 的模块缓存 `/gomod`、Python 的 `/deps`；Node 的 `node_modules` 在工作副本里）。
3. **运行 Lint**：另一个独立容器，同样断网。即使测试失败也会继续运行 Lint，两者结果分开记录。

每个容器：内存 `sandbox_memory`（默认 1g，且不允许使用 swap）、CPU `sandbox_cpus`（默认 1）、最多 256 个进程、去掉所有 Linux capabilities、禁止提权、以非 root 用户（65534）运行、根文件系统只读（仅 `/tmp` 可写）、超时 `sandbox_timeout`（默认 5m）后强制终止并删除。

代码使用**临时工作副本**：不会直接挂载 checkout 目录，并且**不复制 `.git`**（`actions/checkout` 默认会把 GitHub Token 写在 `.git/config` 里，复制进去就等于让 PR 代码读到令牌）。符号链接按原样保留，不会被跟进去读取。

### commit status

写到 PR 的 **head 提交**（不是 `pull_request` 默认检出的合并提交），名称 `sandbox-tests`，并带有指向本次运行日志的链接。

同一个仓库里跑多个沙盒 job（例如 Go、Node 各一个）时，请给它们设置不同的 `sandbox_status_context`。否则它们共用同一个状态名和同一条评论，后跑完的会覆盖先跑完的结果。

| 情况 | state | description |
|---|---|---|
| 开始执行 | `pending` | Sandbox checks running |
| 测试和 Lint 都通过 | `success` | All sandbox checks passed |
| 测试或 Lint 没通过（退出码非 0） | `failure` | Sandbox checks failed |
| Docker 异常、依赖准备失败、超时、内存溢出（含疑似） | `error` | Sandbox execution error |

没通过是 `failure`，沙盒自身出问题是 `error`，两者不会混在一起：环境问题不会被算成代码问题。

关于内存溢出：Docker 的 `OOMKilled` 标志在部分环境里会随机丢失（内核日志显示 OOM kill 确实发生了，Docker 却报告 false）。所以容器以退出码 137（SIGKILL）结束、且不是因为超时时，一律按 `error` 处理，总评里写“疑似内存溢出”。代价是测试代码自己执行 `kill -9` 也会被归为 `error`；这是有意的取舍，宁可提示资源问题，也不把它当成测试失败。

### 命令和镜像

- `sandbox_test_command` / `sandbox_lint_command` / `sandbox_install_command` 由你在 workflow 里写死，**绝不接受模型生成的命令**。不填时按“支持的语言”一节取默认值。
- `sandbox_image` 不填时按项目类型选择（Go 看 `go.mod`，Node 看 `.nvmrc` / `.node-version` / `engines.node`，Python 看 `.python-version` / `requires-python`）。默认镜像是官方的精简镜像，常见的测试工具不一定预装：Python 的默认 test 用的是 `pytest`，镜像里没有，需要用 `sandbox_test_command` 换成 `python -m unittest`，或者在 `sandbox_install_command` 里把它一起装上。
- `sandbox_network` 目前只接受 `none`。

### 安全边界与残余风险

- 容器拿不到 `GITHUB_TOKEN` 和模型 API Key：宿主机环境变量不会传入，没有 `--env-file`，没有挂载 `docker.sock`，容器里也无法再启动别的容器。
- **沙盒 job 里不要配置模型密钥**。推荐像上面那样把沙盒放在单独的 job 里。
- **fork 的 PR**：请继续使用 `pull_request` 事件（fork 拿不到 secrets，且 `GITHUB_TOKEN` 是只读的，写状态和评论会失败并只打印警告）。**不要**为了让 fork 能写状态而改用 `pull_request_target` 再检出 fork 代码，那会让不可信代码在有写权限和密钥的环境里运行。
- 隔离依赖的是 Docker 的默认机制（namespace、cgroup、capability、seccomp），**没有** gVisor、Firecracker 这类额外的沙盒。容器逃逸漏洞仍是残余风险。对完全不信任的来源，建议不要启用，或只在一次性的自托管 runner 上启用。
- 准备依赖的那一步是联网的。它受资源限制、没有密钥，也不执行 PR 代码（Go 的 `go mod download`、Node 的 `--ignore-scripts`、Python 的 `--only-binary`），但依赖本身会从对应的包源（Go 模块代理、npm、PyPI）下载。如果你用 `sandbox_install_command` 覆盖了默认值，这一步就按你写的命令执行，**可能执行 PR 里的代码**，配置前请确认。

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
