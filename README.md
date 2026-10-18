# Ollupi

Ollupi 是一个 Go 写的 agent 运行时库：ReAct 循环、会话与调度、工具注册、权限与审批、沙箱、脱敏、记忆和网关协议。它负责把模型、工具、记忆、权限和沙箱组织成一条可以完整回放的执行链路。任何人都可以 import 它，接上自己的工具、技能和界面，做出自己的 agent 软件。

[haoagent](https://github.com/0xfakeSpike/haoagent) 是用 Ollupi 构建的参考应用：macOS 桌面端、Shell / MCP / OpenCLI / Skills 工具、语音外呼调查等都在那里。

## 核心机制

下面按独特程度排列，是 Ollupi 和其他 agent 运行时区别最大的部分。

### 1. Turn 编排：一条队列、4 个并发 worker、子代理派生

- 所有 thread 的 turn 共用一条全局 FIFO 队列，最老的最先跑，最多 4 个 worker 并发消费。
- turn 在两个时机入队：刚启动时，以及它等待的东西到位时——用户回了它的问题，或者它派生的所有子代理都交了报告。
- 暂停的 turn 不占 worker，所以互相等待的 turn 不会饿死 worker。
- Agent 工具调用会暂停父 turn，为每个调用开一个独立子代理线程；子代理跑完把最终报告写回那条调用，父 turn 在所有报告齐了之后重新入队。子代理不继承父对话，prompt 里要带全它需要的东西。

### 2. 权限系统

- 工具和命令规则：deny / ask / allow，可以带匹配串做细粒度控制。
- 审批策略（never / on-request 等）之上，支持会话级授权：批准过一次，同一命令不再问。
- 跑出沙箱算提权，需要单独批准；审批策略可以完全禁止提权。
- 审批前先冻结要执行的目录、命令、环境变量，批准后按冻结的内容执行。

### 3. 敏感数据处理

- 所有进模型的内容在模型边界统一脱敏：手机号、身份证、银行卡、邮箱、密钥等识别后替换成占位符。
- 占位符确定性生成，同一值始终是同一占位符；turn 暂停恢复后仍能还原。
- 只有声明需要真实值的工具（如拨号、写回 Excel）在执行前把占位符还原，其余工具只见占位符。

### 4. 执行过程全链路记录

- 运行时没有隐藏状态：模型文本、推理、每次工具调用、审批、计划、提问与回答、工具进度，都先落成一条 thread item 再执行。
- 每条响应记录 token 用量和上下文统计，整个会话可以按原样回放。
- 这份原始轨迹既喂给记忆构建，也是以后做 eval 的原始语料。

### 5. 生成式记忆

- SQLite 存储，独立于 agent 运行时轮询，不通过工具调用。
- 用 LLM 从原始轨迹里增量提炼记忆，和已有记忆合并（创建 / 更新 / 废弃），不重复。
- 按工作区或全局召回，用户可编辑、可遗忘，带修订版本；用户手改过的条目受保护，不会被自动覆盖。

## 其他能力

- `update_plan` 计划与进度、`request_user_input` 结构化提问、`Agent` 子代理工具。
- 工具注册表：应用实现 `tools.Provider`（可选 `PermissionContentProvider`、`ExecutionProvider` 等接口）接入自己的工具；注册表负责参数校验、分发和注解（只读、破坏性、访问外部）。
- 内置 Cron 定时任务，绑定项目工作区，持久化在 SQLite。
- 子目录 `AGENTS.md` 在命令第一次进入时加载，跟随会话、压缩后保留。

## 用法

Ollupi 是 Go 库，不是可执行程序：

```sh
go get github.com/0xfakeSpike/ollupi
```

应用自己负责装配：读配置、创建存储、注册工具、构建 Runner 和 Manager，再按需要挂上网关和界面。haoagent 的 `appserver/wire.go` 是一份完整的装配示例，可以照着改。

## 包结构

- `agent` — ReAct 运行时：循环、工具调用、审批、权限规则、子代理定义；`agent/contextmanager` 拼提示词与压缩。
- `thread` / `threadmanager` — 会话数据模型（Thread / Turn / Item）、持久化恢复、turn 调度与子代理。
- `tools` — 工具注册表与 provider 接口。
- `execution` — turn 的执行环境快照、命令冻结记录、子目录说明文件。
- `sandbox` — macOS Seatbelt 沙箱：策略、profile、域名代理、进程监管、主机进程运行。
- `memory` / `redact` — 生成式记忆、模型边界脱敏。
- `gateway` / `jsonrpc` — WebSocket 上的 JSON-RPC 2.0 网关，方法名对齐 Codex app-server。
- `cron` — 定时调度。
- `storage` / `sqlite` — SQLite 存储；`workspace` — 项目工作区。
- `llm` — OpenAI 兼容 Chat Completions 客户端与模型配置。

## 与 Codex / Claude Code 的能力对照

完整的逐项能力对照见 [docs/agent-capabilities.md](docs/agent-capabilities.md)。
