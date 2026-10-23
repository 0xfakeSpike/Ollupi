# Agent 能力清单：Codex、Claude Code 与 Ollupi 对照

本文按 agent 能力框架列出一个 agent 应具备的能力，并逐项标出 Codex、Claude Code 和 Ollupi 的实现情况，用于规划 Ollupi 后续要补的能力。

## 框架来源

能力分类综合了三个被广泛引用的框架，再加上生产级 agent（Codex、Claude Code）都具备的运行时能力：

- OpenAI《A practical guide to building agents》（2025）：模型、工具、指令三要素，以及编排、护栏、人工介入。
- Lilian Weng《LLM Powered Autonomous Agents》（2023）：规划、记忆、工具使用。
- Anthropic《Building effective agents》（2024）：以"增强型 LLM"（检索、工具、记忆）为基础，强调简单可组合的模式和人工检查点。

据此分为 13 个领域：

1. 模型
2. 指令与上下文
3. 规划与推理
4. 工具使用
5. 记忆
6. 编排
7. 护栏与安全
8. 人机协作
9. 会话与状态
10. 接口与集成
11. 自动化
12. 扩展与定制
13. 可观测与运维

## 标记说明

| 标记 | 含义 |
| --- | --- |
| 有 | 已实现，可直接使用 |
| 部分 | 有相关实现，但覆盖不全或较弱，见备注 |
| 无 | 未实现 |
| ? | 不确定：功能可能在较新版本中加入，未能确认 |

## 对照基准

- **Codex**：OpenAI 的开源 Codex CLI，包括 TUI、`codex exec`、app-server 和 IDE 插件；不含 Codex 云端任务，除非备注说明。
- **Claude Code**：Anthropic 的 Claude Code，包括 CLI、headless 模式、Agent SDK 和 IDE 插件。
- **Ollupi**：本仓库当前代码，已逐项对照源码确认，"位置"一列给出实现所在的包。

Codex 和 Claude Code 两列来自公开文档和发布说明，两者更新很快，可能落后于最新版本，标"?"的项尤其需要复核。

---

## 1. 模型

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 1.1 | 原生工具调用 | 用模型 API 的 function/tool calling，而非解析文本 | 有 | 有 | 有 | `pkg/llm`，OpenAI Chat Completions 的 tools 字段 |
| 1.2 | 多模型提供方 | 可接入不同厂商或本地模型 | 有：`model_providers`，支持任意 OpenAI 兼容端点、Ollama | 部分：Anthropic API、Bedrock、Vertex；其他厂商需代理 | 有 | 多套 OpenAI 兼容连接，各自保存模型、API 地址与 Key；设置页管理，新增或修改配置后重启加载 |
| 1.3 | 运行中切换模型 | 按会话或按 turn 换模型 | 有：`/model`，turn 级覆盖 | 有：`/model` | 有 | 桌面输入框按轮次切换；运行中的 turn 锁定模型、连接 ID 与 thinking，下一轮可换；`turn/start.connection` |
| 1.4 | 推理强度控制 | reasoning effort、thinking budget | 有：`model_reasoning_effort` | 有：extended thinking | 部分 | 只有 `model_settings.enable_thinking` 开关（Qwen 扩展） |
| 1.5 | 推理内容回放 | 把模型的推理内容按协议原样带回下一次请求 | 有 | 有 | 有 | reasoning item 按 `responseId` 回放为 `reasoning_content` |
| 1.6 | 流式输出 | 逐 token 推送文字 | 有 | 有 | 有 | 模型以 SSE 流式返回；`item/agentMessage/delta`、`item/reasoning/textDelta` 推送增量，item 完成时带全文（`llm`、`agent`） |
| 1.7 | 多模态输入 | 图片、截图作为输入 | 有：`-i`、粘贴图片 | 有：粘贴、拖拽图片 | 无 | `turn/start` 只接受 text |
| 1.8 | 请求失败重试 | 网络、限流错误自动重试 | 有 | 有 | 部分 | 依赖 openai-go SDK 默认重试；单次请求超时 180 秒 |
| 1.9 | 提示词缓存友好 | 系统提示和工具列表稳定，命中前缀缓存 | 有 | 有 | 有 | 每个 turn 工具列表字节级一致；压缩请求复用同一前缀 |
| 1.10 | 输出长度截断处理 | 识别 `finish_reason=length` | 有 | 有 | 有 | 截断的最终回答直接报错，不当成完整答案 |

## 2. 指令与上下文

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 2.1 | 内置系统指令 | 循环规则、安全规则、回答规范 | 有 | 有 | 有 | `agent/systemprompt`:system message 的全部组成(agent 自己的 prompt、子代理说明、循环规则、执行环境、AGENTS.md、技能目录)都在这里拼 |
| 2.2 | Agent 自定义指令 | 每个 agent 自己的 instructions | 部分：`developer_instructions`、profile | 有：subagent 的提示词、`--append-system-prompt` | 有 | 配置 `agent.instructions` |
| 2.3 | 工作区说明文件 | `AGENTS.md` / `CLAUDE.md` | 有：`AGENTS.md` | 有：`CLAUDE.md` | 无（有意不做） | 不读用户目录里的说明文件；agent 的 prompt 由应用给出，留空用代码里的默认范本（`agent/systemprompt`），用户偏好靠长期记忆 |
| 2.4 | 说明文件分层 | 全局、项目、子目录多级合并 | 有：`~/.codex`、仓库根、当前目录 | 有：企业、用户、项目、本地、子目录 | 无（有意不做） | 同 2.3 |
| 2.5 | 说明文件互相引用 | 在说明文件里 `@` 引用其他文件 | 无 | 有：`@path` 导入 | 无 | |
| 2.6 | 环境上下文注入 | 当前目录、日期、操作系统、git 状态等 | 有 | 有 | 部分 | 轮次注入项目根、默认执行目录和执行后端；定时工具提供时间和时区，未自动注入 Git 状态 |
| 2.7 | 引用文件或资源 | 在输入里 `@文件`、`@MCP 资源` | 有：`@` 搜索文件 | 有：`@文件`、`@MCP 资源` | 无 | |
| 2.8 | 工具输出截断 | 发给模型时截断超长输出，存储保留全文 | 有 | 有 | 有 | 每个结果最多 6000 token（随结果带回的技能说明也算在内，输出文字用剩下的），库里存全文（`agent/contextmanager`） |
| 2.9 | 上下文预算估算 | 估算请求大小，避免超窗 | 有：按真实 token 用量 | 有 | 有 | 预算是本 turn 所用模型的输入上限（`pkg/llm` 按连接声明和内置模型表给出）；按字符估算系统提示、对话、实际工具 schema，再用同一模型在上次压缩后最近一次请求的真实 prompt tokens 加上之后的估算增量校准；启动时检查系统提示和工具定义不超过上限的一半 |

## 3. 规划与推理

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 3.1 | ReAct 循环 | 推理、调用工具、观察，循环到完成 | 有 | 有 | 有 | `agent/queue.go`（推进循环）、`agent/runtime`（一次迭代） |
| 3.2 | 计划工具 | 模型维护带状态的步骤清单 | 有：`update_plan` | 有：TodoWrite | 有 | `update_plan`（Codex 格式），普通工具（`agent/tools/plan`），计划就是调用参数 |
| 3.3 | 计划模式 | 只调研规划、不执行，确认后再做 | ? | 有：plan mode | 无 | |
| 3.4 | 停止条件 | 模型不再调用工具即完成 | 有 | 有 | 有 | |
| 3.5 | 循环上限 | 限制模型调用次数、工具调用次数 | 有 | 有 | 有 | 每个 turn 模型调用 32 次、工具调用 16 次，超限后强制收尾 |
| 3.6 | 失败自我纠正 | 读错误、换参数或换策略 | 有 | 有 | 有 | 拒绝完全相同的失败调用；连续失败 3 次后停止工具调用 |
| 3.7 | 强制收尾 | 停止工具调用后要求模型给出总结 | 有 | 有 | 有 | error item 加 `tool_choice: none` |
| 3.8 | 自我审查与反思 | 完成后自检、代码审查 | 有：`/review` | 有：`/review`、安全审查 | 无 | |

## 4. 工具使用

### 4.1 内置工具

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 4.1.1 | 执行命令 | 运行任意程序 | 有 | 有：Bash | 有 | `run_command`（`agent/tools/shell`） |
| 4.1.2 | 后台进程 | 长时间运行的命令，之后再读取输出 | 部分 | 有：后台 Bash、读取后台输出 | 无 | 命令有超时，必须同步结束 |
| 4.1.3 | 读文件 | 专用读文件工具，支持分页 | 无：通过命令行 | 有：Read | 无 | 只能用 `run_command` |
| 4.1.4 | 写文件、编辑文件 | 专用写入、精确替换、补丁 | 有：`apply_patch` | 有：Write、Edit、MultiEdit | 无 | |
| 4.1.5 | 文件和代码搜索 | 按名字或内容搜索 | 无：通过 `rg` 命令 | 有：Glob、Grep | 无 | |
| 4.1.6 | 联网搜索 | 搜索引擎 | 有：可选 `web_search` | 有：WebSearch | 有 | OpenCLI 在用户自己的 Chrome 里搜索 Google、Brave、DuckDuckGo，`web-search` 技能定义优先顺序和被拦截时的退路 |
| 4.1.7 | 抓取网页 | 取网页内容 | ? | 有：WebFetch | 有 | OpenCLI `web read`：真实浏览器渲染（含 JS 页面），正文转 Markdown 直接返回，不落盘 |
| 4.1.8 | 查看图片 | 让模型看本地图片 | 有：`view_image` | 有：Read 支持图片 | 无 | |
| 4.1.9 | Notebook 编辑 | Jupyter 单元格编辑 | 无 | 有：NotebookEdit | 无 | |
| 4.1.10 | 网站自动化 | 浏览器或站点命令行操作网站 | 无 | 部分：通过 MCP | 有 | OpenCLI 工具集，按允许列表从命令帮助生成 schema（haoagent `tools/opencli`） |
| 4.1.11 | 定时任务工具 | 让 agent 自己创建、删除、列出定时任务 | 无 | 无 | 有 | `create_scheduled_job` 等（`agent/tools/cron`），作用于当前会话；界面也通过 `tool/call` 直接调用它们来列出、暂停、删除 |
| 4.1.12 | 工具自省 | 列出可用的工具提供方和工具 | 无 | 无 | 有 | `list_tool_providers`、`list_tools_for_provider` |

### 4.2 工具机制

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 4.2.1 | 参数 JSON Schema 校验 | 执行前校验参数 | 有 | 有 | 有 | 执行前完整校验 JSON Schema |
| 4.2.2 | 工具属性声明 | 只读、破坏性、访问外部 | 部分 | 部分 | 有 | MCP `readOnlyHint`、`destructiveHint`、`openWorldHint`，审批据此决定 |
| 4.2.3 | 并行工具调用 | 一次回复里多个调用并发执行 | ? | 有：只读工具并发 | 无 | 普通工具逐个执行；只有 `Agent` 调用并行 |
| 4.2.4 | 工具结果结构化保存 | 完整参数、输出、错误入库 | 部分：rollout 文件 | 部分：会话文件 | 有 | mcpToolCall item：参数原文、完整输出、属性、审批 |
| 4.2.5 | 限定 agent 可用工具 | 每个 agent 只给部分工具 | 部分 | 有：`allowedTools`，subagent 的 tools | 有 | `agent.tools` 允许列表，启动时校验 |

### 4.3 MCP

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 4.3.1 | MCP 客户端：工具 | 调用 MCP 服务器的工具 | 有 | 有 | 有 | `agent/tools/mcp` |
| 4.3.2 | MCP 传输：本地进程（stdio） | 启动本地 MCP 进程 | 有 | 有 | 无 | |
| 4.3.3 | MCP 传输：HTTP | Streamable HTTP、SSE | 有 | 有 | 部分 | 只支持 Streamable HTTP |
| 4.3.4 | MCP 认证 | OAuth 等 | 部分 | 有 | 无 | |
| 4.3.5 | MCP 资源和提示词 | resources、prompts | 无 | 有 | 无 | |
| 4.3.6 | 作为 MCP 服务器 | 把 agent 暴露给其他 MCP 客户端 | 有：`codex mcp-server` | 有：`claude mcp serve` | 无 | |
| 4.3.7 | MCP 管理命令 | 增删改 MCP 服务器配置 | 有：`codex mcp` | 有：`claude mcp` | 无 | 只能改 `mcp_endpoint` 配置 |

## 5. 记忆

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 5.1 | 短期记忆：完整会话历史 | 同一会话内连续上下文 | 有 | 有 | 有 | thread 内的全部 item 重建为对话记录 |
| 5.2 | 自动压缩 | 快满时由模型摘要 | 有 | 有 | 有 | 超过所用模型输入上限的 80%（按校准后的大小）时压缩，记为 contextCompaction item；压缩请求超限时整段丢最早的 turn，但保留上一次的摘要 |
| 5.3 | 手动压缩 | 用户触发压缩，可附要求 | 有：`/compact` | 有：`/compact [指令]` | 无 | |
| 5.4 | 压缩后保留关键内容 | 保留用户原话、已加载的指令 | 有：保留近期用户消息 | 有 | 有 | 保留近期用户原话（10% 预算）和已激活技能的说明（合计 15% 预算，优先最近激活的，放不下的只留名字并提示重新加载） |
| 5.5 | 长期记忆：写入 | 把偏好、事实记下来供以后用 | ? | 有：`#` 快捷写入、`/memory` | 有 | 独立 MemoryManager 在 turn 结束后扫描轨迹增量，由 LLM 提炼 / 修订；正文、来源和进度保存在 SQLite，条目不带版本号；Memory 页面可编辑、忘记 |
| 5.6 | 长期记忆：跨会话召回 | 新会话自动带上记忆 | 部分：靠 `AGENTS.md` | 有：靠 `CLAUDE.md` 记忆 | 有 | 按全局 / 工作区检索，含中文词组匹配；每轮开始冻结一次记忆，预算为输入的 5%，最多 2000 tokens |
| 5.7 | 检索增强 | 向量检索、知识库 | 无 | 无 | 无 | |

## 6. 编排

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 6.1 | 单 agent 循环 | 一个 agent 处理整个任务 | 有 | 有 | 有 | |
| 6.2 | 可配置的 agent 定义 | 名称、指令、模型、工具、审批策略 | 部分：profile | 有：subagent 定义 | 有 | 配置 `agent` 块（`agent/agent.go`） |
| 6.3 | 多套 agent 定义 | 同时存在多个 agent 可选 | 部分：多个 profile | 有：`.claude/agents/` | 无（有意不做） | 只有一个 agent；派生的子任务也是它自己，用同样的 prompt、工具和模型 |
| 6.4 | 子代理 | 主 agent 把子任务交给子 agent，独立上下文 | 有：`spawn_agent` | 有：Agent（原 Task）工具 | 有 | `Agent` 工具（Claude Code 前台方式，`agent/tools/subagent`）：普通工具，结果是 `Await`（由另一个 agent 回答），agent 为每个调用新建子 thread；父 turn 暂停、不占 worker；子 turn 结束时同一个事务把最终回答写成那条调用的结果，全部到齐后父 turn 继续。派生的 turn 就是同一个 agent，只能派一层（deny 掉 `Agent`），父 turn 停止时连带停止 |
| 6.5 | 并行子代理 | 多个子 agent 同时工作 | 有 | 有 | 有 | 一次回复里的多个 `Agent` 调用同时运行（受全局 4 个 worker 限制）；子代理的批准请求在父对话里排队，逐个处理 |
| 6.6 | Agent 间交接（handoff） | 把对话交给另一个 agent | 无 | 无 | 无 | OpenAI Agents SDK 的概念 |
| 6.7 | 多会话并发 | 多个会话同时运行 | 有 | 有 | 有 | 每个 thread 同时一个 turn；调度器把可运行的 turn 放进一个先进先出队列，最多 4 个 worker 同时运行，暂停中的 turn 不占 worker（`agent/queue.go`） |

## 7. 护栏与安全

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 7.1 | 审批策略 | 哪些调用需要人批准 | 有：`untrusted`、`on-failure`、`on-request`、`never` | 有：default、acceptEdits、plan、bypassPermissions | 有 | `untrusted`、`on-request`、`never`（`agent/permission/policy.go`） |
| 7.2 | 按会话覆盖审批策略 | 单个会话用不同策略 | 有 | 有 | 有 | thread 的 `approvalPolicy`；解析顺序是 会话 > 工作区 > agent 默认 |
| 7.3 | 允许、拒绝规则 | 按工具和参数模式配置 | 部分：命令执行规则 | 有：`Bash(git:*)` 等 | 有 | agent 默认的 allow、ask、deny 和工作区的 `permissions` 字段，语法同 Claude Code：`工具名` 或 `run_command(git diff *)`；按 deny、ask、allow 顺序第一个匹配生效，deny 单写工具名时不提供该工具；规则写错启动即报错（`agent/permission/rules.go`；判断顺序见 `permission.Decide`）；解析出的规则随 turn 一起快照 |
| 7.4 | "本次会话一直允许" | 批准一次，后续同类不再询问 | 有 | 有：不再询问 | 有 | Codex 的 `acceptForSession`：同一 thread 里相同命令（其他工具为同一工具）不再询问，记在审批 item 上，重启后仍有效；ask 规则拦下的调用不提供此选项 |
| 7.5 | 系统级沙箱 | 隔离文件系统和网络 | 有：Seatbelt、Landlock、seccomp | 部分：可选沙箱 | 部分 | 只有 macOS Seatbelt 一套：按策略生成默认拒绝的 profile，基础规则取自 Codex；工作目录内拒读凭据、git 元数据只读、开网时关闭本机 Unix socket；非 macOS 上沙箱模式启动即报错，只能显式选 `full-access`（`pkg/sandbox`、`agent/tools/shell`）。（haoagent 的 OpenCLI 曾经通过 `sandbox.RunHost` 在沙箱外运行；Ollupi 里没有使用方，这个入口已删除） |
| 7.6 | 沙箱分级 | 只读、可写工作区、完全放开 | 有 | 部分 | 有 | `exec.sandbox_mode`：`read-only` / `workspace-write`（默认）/ `full-access`（不使用沙箱） |
| 7.7 | 沙箱与审批联动 | 沙箱内放行，越界才申请 | 有：on-request 升级 | 部分 | 有 | on-request 下沙箱内命令免审批；`run_command` 带 `sandbox_permissions: "require_escalated"` 和 `justification` 申请在沙箱外执行，需审批；`never` 拒绝升级（`agent/permission`、`agent/tools/shell`） |
| 7.8 | 网络访问控制 | 默认断网、按需开放 | 有 | 部分 | 有 | `exec.network` 开关（关闭时连 localhost 一并禁止），默认断网；`exec.allowed_domains` 按域名放行（每条命令一个本地 HTTP/SOCKS5 代理，Seatbelt 只放行代理端口，参照 sandbox-runtime 与 Codex）；开网时仍关闭本机 Unix socket（Docker、ssh-agent），只放行 `exec.unix_sockets` |
| 7.9 | 工作目录限制 | 命令只能在指定目录运行 | 有 | 有 | 有 | `exec.workspace_dirs` |
| 7.10 | 危险命令拦截 | 黑名单、已知安全命令 | 有：安全命令白名单 | 有：规则 | 部分 | `command_blacklist` 和 deny 规则；没有内置的已知安全命令清单 |
| 7.11 | 提示注入防护 | 工具输出只当数据 | 部分：靠沙箱 | 部分：靠权限 | 部分 | 只在系统提示里约定 |
| 7.12 | 秘密信息保护 | 不泄露密钥 | 部分 | 部分 | 部分 | 命令和 OpenCLI 都只继承环境变量白名单（`exec.env_passthrough` 显式追加）；Seatbelt 下凭据目录、应用 `config.json` 和数据库禁止读写 |
| 7.13 | 网关访问控制 | 对外接口的认证 | 部分：主要以本地进程（stdio）方式提供 | 部分：本地进程 | 有 | 令牌认证（桌面应用每次启动生成随机令牌和端口），浏览器限本地来源 |

## 8. 人机协作

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 8.1 | 工具调用审批 | 执行前请求批准，可拒绝 | 有 | 有 | 有 | `item/mcpToolCall/requestApproval`，带 `reason`、`availableDecisions`（accept、acceptForSession、decline） |
| 8.2 | 向用户提问 | 结构化问题加选项 | ? | 有：AskUserQuestion | 有 | `request_user_input`（`agent/tools/userinput`）：1 到 3 个问题，可带选项；结果是 `Await` 给用户，回答成为调用结果。不挡同一回复里的其他调用（例如同时派出的子代理） |
| 8.3 | 等待中的请求可跨重启 | 进程重启后审批、提问仍有效 | 无 | 无 | 有 | turn 保持进行中，`thread/resume` 重发请求（子代理的请求一并重发） |
| 8.4 | 中断 | 停止当前 turn | 有 | 有 | 有 | `turn/interrupt` |
| 8.5 | 插话、排队 | 运行中追加指示或排队下一条消息 | 有 | 有 | 有 | `turn/steer`：运行中发的消息在下一次模型调用前进入当前 turn，回答途中收到也会接着处理；桌面端 Tab 排队，当前 turn 完成后作为下一个 turn 发送，停止时退回输入框 |
| 8.6 | 编辑并重发历史消息 | 回到某条消息改了重来 | 有：回退编辑 | 有：rewind | 无 | |
| 8.7 | 进度可见 | 实时看到 agent 在做什么 | 有 | 有 | 有 | item 事件加流式文字和推理增量 |
| 8.8 | 完成通知 | turn 结束时提醒 | 有：notify | 有：Notification hook | 部分 | `turn/completed` 事件，没有系统通知 |

## 9. 会话与状态

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 9.1 | 会话持久化 | 关闭后会话还在 | 有：rollout JSONL | 有：会话 JSONL | 有 | SQLite threads、turns、items（`repo`） |
| 9.2 | 恢复会话 | 继续之前的会话 | 有：`codex resume` | 有：`--resume`、`--continue` | 有 | `thread/resume` |
| 9.3 | 分叉会话 | 从某点分出新会话 | 有 | 有：`--fork-session` | 无 | |
| 9.4 | 检查点、撤销 | 撤销 agent 对文件的改动 | 有：`/undo` | 有：checkpoint、rewind | 无 | |
| 9.5 | 会话列表与检索 | 列出、筛选历史会话 | 有 | 有 | 部分 | `thread/list` 带状态，`thread/status/changed` 实时推送；没有搜索 |
| 9.6 | 会话命名、归档、删除 | 管理历史会话 | 部分：归档 | 部分 | 无 | |
| 9.7 | 完整执行记录 | 每一步可追溯 | 有 | 有 | 有 | 每个 item 带状态、时间、完整参数和输出 |
| 9.8 | 表结构版本 | 存储格式可演进 | 不适用 | 不适用 | 无 | 开发期不做版本管理:gorm 按行结构体建表、补列,格式不兼容时重建数据库 |

## 10. 接口与集成

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 10.1 | 终端交互界面 | TUI | 有 | 有 | 无 | |
| 10.2 | 非交互执行 | 一次性执行，适合脚本和 CI | 有：`codex exec`，可输出 JSON | 有：`-p`，可输出 stream-json | 无 | |
| 10.3 | 对外协议 | 客户端可编程接入 | 有：app-server JSON-RPC | 有：SDK 和 stream-json | 有 | WebSocket JSON-RPC，方法名与 Codex app-server 对齐（`thread/*`、`turn/start`、`turn/steer`、`turn/interrupt`，`gateway`） |
| 10.4 | 服务端主动请求 | 审批、提问由服务端发给客户端 | 有 | 有：SDK 回调 | 有 | |
| 10.5 | 官方 SDK | 编程语言 SDK | 有：TypeScript | 有：TypeScript、Python | 无 | |
| 10.6 | IDE 插件 | VS Code、JetBrains | 有 | 有 | 无 | |
| 10.7 | Web 或桌面客户端 | 图形界面 | 有：云端网页 | 有：网页版 | 有 | macOS 桌面应用（Wails v2 + React），后端在同一进程内（`desktop/`） |
| 10.8 | 消息平台接入 | Telegram、Slack、飞书等 | 无 | ? | 无 | Telegram 已删除 |
| 10.9 | 代码托管集成 | GitHub PR、Issue | 有：云端代码审查 | 有：GitHub Actions | 无 | |

## 11. 自动化

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 11.1 | 定时任务 | 按 cron 计划自动运行 | 无 | 无 | 有 | 任务属于创建它的会话，每次运行是那个会话的一个 turn；到期时间由主调度器检查，会话忙就等当前 turn 结束（`agent/cron.go`） |
| 11.2 | 定时任务结果投递 | 把结果推送到指定渠道 | 无 | 无 | 无 | 随 Telegram 删除 |
| 11.3 | Hooks | 在工具调用前后、会话开始和结束等时机执行自定义逻辑 | 部分：notify | 有：PreToolUse、PostToolUse、Stop 等 | 无 | |
| 11.4 | 事件触发 | Webhook、文件变化等触发运行 | 无 | 部分：GitHub Actions | 无 | |
| 11.5 | 无人值守运行 | 无人审批时安全运行 | 有：`exec` 加沙箱 | 有：headless 加权限规则 | 部分 | 定时任务的审批会一直等人处理 |

## 12. 扩展与定制

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 12.1 | 技能（Agent Skills） | 按需加载的 `SKILL.md` 和资源 | ? | 有 | 有 | 目录、激活、读取资源（`agent/skills`） |
| 12.2 | 技能声明可用工具 | `allowed-tools` | ? | 有：强制执行 | 部分 | 只展示给模型看，不强制 |
| 12.3 | 自定义斜杠命令、提示词模板 | 可复用的提示词 | 有：custom prompts | 有：`.claude/commands` | 无 | |
| 12.4 | 插件 | 打包分发命令、agent、hooks、MCP | 无 | 有：插件市场 | 无 | |
| 12.5 | 配置档（profile） | 多套配置一键切换 | 有 | 部分 | 无 | |
| 12.6 | 分层配置 | 全局、项目、本地配置合并 | 有 | 有 | 无 | 只有应用数据目录中的全局 `config.json`；项目指令独立加载 |
| 12.7 | 输出风格 | 调整回答风格 | 无 | 有：output styles | 部分 | 通过 `agent.instructions` |
| 12.8 | 状态栏定制 | 自定义终端状态显示 | 部分 | 有 | 不适用 | 没有终端界面 |

## 13. 可观测与运维

| # | 能力 | 说明 | Codex | Claude Code | Ollupi | Ollupi 现状与位置 |
| --- | --- | --- | --- | --- | --- | --- |
| 13.1 | 运行日志 | 文件日志 | 有 | 有 | 有 | `workspace/logs/agent.log`，每个 turn 有调用追踪 |
| 13.2 | 链路追踪与指标 | OpenTelemetry | 有 | 有 | 无 | |
| 13.3 | token 用量 | 按调用和 turn 统计 | 有 | 有 | 有 | 每次 LLM 调用在一个 item 中保存 system/messages/tools 输入估算与供应商返回的 input、cached、output 用量；turn 保留累计用量 |
| 13.4 | 费用估算 | 换算成金额 | 部分 | 有：`/cost` | 无 | |
| 13.5 | 配置校验 | 启动时检查配置错误 | 有 | 有 | 有 | 配置错误、非法策略、未知工具名启动即报错 |
| 13.6 | 优雅关闭 | 关闭时保存状态、通知客户端 | 有 | 有 | 有 | 关闭帧 1001；进行中的 turn（父子都算）保持进行中，下次启动时调度器按开始时间恢复并继续；只有停止时正在执行的那个工具调用记为"可能已生效"的失败，交给模型判断 |
| 13.7 | 版本信息 | 客户端可知服务端版本 | 有 | 有 | 有 | `initialize` 返回 serverInfo |
| 13.8 | 评测 | 内置评测或基准测试工具 | 无 | 无 | 无 | |

---

## 统计

| 领域 | 能力项 | Ollupi 有 | 部分 | 无 |
| --- | --- | --- | --- | --- |
| 1 模型 | 10 | 7 | 2 | 1 |
| 2 指令与上下文 | 9 | 4 | 1 | 4 |
| 3 规划与推理 | 8 | 6 | 0 | 2 |
| 4 工具使用 | 24 | 11 | 1 | 12 |
| 5 记忆 | 7 | 5 | 0 | 2 |
| 6 编排 | 7 | 5 | 0 | 2 |
| 7 护栏与安全 | 13 | 9 | 4 | 0 |
| 8 人机协作 | 8 | 6 | 1 | 1 |
| 9 会话与状态 | 8 | 3 | 1 | 4 |
| 10 接口与集成 | 9 | 3 | 0 | 6 |
| 11 自动化 | 5 | 1 | 1 | 3 |
| 12 扩展与定制 | 8（含 1 项不适用） | 1 | 2 | 4 |
| 13 可观测与运维 | 8 | 5 | 0 | 3 |
| **合计** | **124** | **66** | **13** | **44** |

另有 1 项不适用（12.8 状态栏定制），未计入"有、部分、无"。按可适用的 123 项计，Ollupi 已实现约 54%，部分实现约 11%，未实现约 36%。

## Ollupi 的独有能力

这些是 Codex 和 Claude Code 都没有（或据了解没有）的：

- **进行中的工作可跨重启**（8.3、13.6）：所有进行中的 turn 都记在 item 上，进程重启后调度器把它们恢复并接着跑；等批准、等回答的继续等，等子代理的父 turn 在子代理跑完后继续。
- **内置定时任务**（4.1.11、11.1）：agent 自己就能创建定时任务；任务是"将来某个时间替用户往这个会话发的一条消息"，每次运行都接着这个会话。
- **工具属性驱动审批**（4.2.2）：直接用 MCP 的只读、破坏性、访问外部属性决定是否审批。
- **网站命令行工具集**（4.1.10）：OpenCLI 命令按允许列表自动生成工具。

## 建议优先补齐

按对个人助理的价值排序：

1. ~~流式输出（1.6）~~ 已完成
2. ~~联网搜索和网页抓取（4.1.6、4.1.7）~~ 已完成
3. ~~运行中插话、排队（8.5）~~ 已完成
4. ~~"本次会话一直允许"和允许、拒绝规则（7.3、7.4）~~ 已完成
5. ~~子代理和多套 agent 定义（6.3、6.4）~~ 已完成，并行子代理（6.5）一并完成
6. ~~长期记忆（5.5、5.6）和说明文件分层（2.4）~~ 已完成
7. Hooks（11.3）
8. 文件读写编辑工具、MCP 本地进程传输、手动压缩、会话管理、费用估算（4.1.3 到 4.1.5、4.3.2、5.3、9.6、13.4）
9. 非交互执行模式（10.2）
