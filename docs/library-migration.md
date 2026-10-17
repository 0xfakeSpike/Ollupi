# Ollupi 库化迁移方案

目标：把 Ollupi 拆成「agent 基础能力库」+「应用层」。Ollupi 成为一个任何人都能 import 的 Go 库，完整实现 agent 运行时能力；shenshiagent 用这个库构建应用，自己带个性化的 tool / MCP / UI。不走 API 调用，是 Go 库 import。

## 包去向

建议模块路径改为小写 `github.com/0xfakeSpike/ollupi`，核心包平铺在模块根下。

| 现在的包 | 去向 | 新 import path |
| --- | --- | --- |
| internal/agent | 核心库 | ollupi/agent |
| internal/agent/contextmanager | 核心库 | ollupi/agent/contextmanager |
| internal/thread | 核心库 | ollupi/thread |
| internal/threadmanager | 核心库 | ollupi/threadmanager |
| internal/memory | 核心库 | ollupi/memory |
| internal/redact | 核心库 | ollupi/redact |
| internal/execution | 核心库 | ollupi/execution |
| internal/gateway | 核心库 | ollupi/gateway |
| internal/cron | 核心库 | ollupi/cron |
| internal/config | 核心库 | ollupi/config |
| internal/storage | 核心库 | ollupi/storage |
| internal/workspace | 核心库 | ollupi/workspace |
| internal/tools（registry 机制） | 核心库 | ollupi/tools |
| internal/tools/shell | 可选子包 | ollupi/tools/shell |
| internal/tools/mcp | 可选子包 | ollupi/tools/mcp |
| internal/tools/skills | 核心库 | ollupi/skills |
| pkg/llm | 核心库 | ollupi/llm |
| pkg/sandbox | 核心库 | ollupi/sandbox |
| pkg/jsonrpc | 核心库 | ollupi/jsonrpc |
| pkg/sqlite | 核心库 | ollupi/sqlite |
| internal/tools/opencli | shenshiagent | — |
| internal/tools/callsurvey | shenshiagent | — |
| internal/appserver | shenshiagent（装配参考） | — |
| pkg/doubao | shenshiagent | — |
| pkg/voice | shenshiagent | — |
| desktop/ | shenshiagent | — |

## 三个决策

1. MCP SDK 去留：核心库定义自己的 `tools.Annotations`，把 `mcp.ToolAnnotations` 从 descriptor / thread 里解耦，MCP SDK 只留在可选子包 `ollupi/tools/mcp`。
2. shell / mcp 作为核心库可选子包，不 import 就不背依赖。
3. 装配入口：把 `internal/appserver/wire.go` 提炼成公开 API `ollupi.New(...)` / `ollupi.Run(...)`，使用方传入自己的 tool provider、skills、memory 配置。

## 稳定公开 API

一旦公开即为承诺：`agent.Runner`、`agent.Agent`、`agent.Turn/Result/Recorder`、`thread.Item/View/Plan/Approval`、`threadmanager.Manager`、`memory.Reader/Entry`、`redact.Redactor`、`tools.Registry/ToolDescriptor/Provider`、`gateway.Server`、`llm.ChatModel`、`execution.Context`。

## 推进顺序

1. 改模块路径，把核心包从 internal/ 机械挪到公开路径（纯改名 + import 替换，保证编译）。
2. 定义自己的 annotations，解耦 MCP SDK。
3. 把 wire 提炼成公开装配 API。
4. 把 app 层（opencli、callsurvey、doubao、voice、desktop）迁到 shenshiagent。
5. 定版本（v0.x + 稳定 API 文档），shenshiagent 用 tag 或 replace 引用。
