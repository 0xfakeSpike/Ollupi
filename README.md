# Ollupi

Ollupi 是一个 Go 写的 agent 运行时库：ReAct 循环、会话与调度、工具注册、权限与审批、沙箱、脱敏、记忆和网关协议。它负责把模型、工具、记忆、权限和沙箱组织成一条可以完整回放的执行链路。任何人都可以 import 它，接上自己的工具、技能和界面，做出自己的 agent 软件。

[haoagent](https://github.com/0xfakeSpike/haoagent) 是用 Ollupi 构建的参考应用：macOS 桌面端、Shell / MCP / OpenCLI / Skills 工具、语音外呼调查等都在那里。

## 核心机制

下面按独特程度排列，是 Ollupi 和其他 agent 运行时区别最大的部分。

### 1. Turn 编排：agent 维护队列，runtime 执行一次迭代

- `agent.Agent` 是整个系统：它自己的 prompt、模型、工具、审批策略、权限规则，加上它维护的会话和 turn 队列。会话管理和 turn 管理都是 agent 自己的方法，没有单独的 turnmanager 结构。
- 队列里是 SQL 中可以前进的 turn。4 个 worker 从队列取 turn，为它建一个 `runtime.Runtime`(这个 turn 的执行器),由 `Step` 执行 ReAct 循环的**一次迭代**:处理上次留下的调用(比如刚被批准的)、召回记忆、拼 prompt、脱敏后调模型、还原、流式输出、判权限、执行工具、把结果写回 SQL。
- 迭代结束时 runtime 报告状态：`Continue` 放回队尾，`Wait` 等用户或子代理，`Done` 结束。循环就是这样由队列一圈圈推进的，runtime 自己不维持循环，也不在两次迭代之间保留任何状态。
- 暂停和恢复不是特殊逻辑：等待中的 turn 不在队列里，也不占 worker；批准、回答、子代理报告一到，它就回到队列。重启后，库里可前进的 turn 就是队列。
- 运行中用户发来的消息(`SteerTurn`)直接写进 SQL，下一次迭代就会读到。
- `Agent` 是一个普通工具，它的结果是"由另一个 agent 回答"(`tools.Await`)。agent 为每个这样的调用开一个子代理会话并让它的 turn 入队；子代理的最终回答写成那条调用的结果，所有回答齐了父 turn 回到队列。子代理不继承父对话，prompt 里要带全它需要的东西。

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

- 内置工具由 agent 在 `Start` 时自己注册,应用不用管:`update_plan`(`agent/tools/plan`,计划与进度)、`request_user_input`(`agent/tools/userinput`,结构化提问)、`Agent`(`agent/tools/subagent`,派生子任务)、定时任务的四个工具(`agent/tools/cron`),以及给了 `SkillsDir` 时的技能工具(`agent/skills`)。它们和应用的工具一样受 `Agent.Tools` 允许列表和权限规则约束。
- 工具注册表：应用实现 `tools.Provider` 接入自己的工具，工具的额外能力（权限匹配、本地执行、是否串行）声明在它的 `tools.Spec` 上；注册表负责参数校验、分发和注解（只读、破坏性、访问外部）。
- 定时任务属于创建它的会话：到点时把任务的消息发到那个会话，作为一个普通 turn 运行，带着会话的历史、项目、模型和审批策略。计时由主调度器负责：每个任务存着下次运行时间，调度器每分钟、以及每个 turn 结束时检查一次；会话正忙就等它的 turn 结束再发。应用关着时错过的运行在启动后补一次。
- 工具既能由 agent 在 loop 里调用，也能由用户通过接口直接调用（gateway 的 `tool/call`）：比如界面上列出、暂停、删除定时任务，调的就是同一组工具。直接调用的调用者是用户本人，所以不走审批和权限规则；需要 turn 执行环境的工具（`run_command`）和要等外部回答的工具不能直接调用。

## 用法

Ollupi 是 Go 库，不是可执行程序：

```sh
go get github.com/0xfakeSpike/ollupi
```

应用自己负责装配：读配置、打开数据库，填好 `agent.Agent` 后调用 `Start`，再按需要挂上网关和界面。要填的是:名字、prompt(可空,用默认范本)、可选模型 `Models`(第一个是默认)和每个连接的模型客户端 `Connections`、审批策略与权限规则;应用自己的工具 `Providers`(比如 shell、MCP);技能目录 `SkillsDir`、长期记忆 `Memory`、定时任务用的时区 `Location`,都可选;数据库 `DB`;以及 `OnEvent`。上下文预算不用填:每个模型能用多少输入 token 由 `pkg/llm` 给出(`OpenAI.Limits`),先看 `llm.NewOpenAI` 收到的该连接上的声明,再看内置的常见模型表,两处都没有的模型 `Start` 会拒绝。工具注册表、定时任务表都由 agent 自己建,记忆学习的后台循环也由 `Start` 启动、`Wait` 等待。haoagent 的 `appserver/wire.go` 是一份完整的装配示例，可以照着改。

## 包结构

一个 agent 由核心 loop 和它调用的下游能力组成。目录就按这个组织:

```
ollupi/
  agent/                    agent 本身:定义、会话、turn 队列与 worker、审批与提问、子代理、定时任务的发送、重启恢复、事件
    runtime/                ReAct 循环的一次迭代:调用处理、请求模型、压缩、流式输出
    permission/             授权:权限规则、审批策略、"本会话都允许",判断一次调用 执行/问/拒绝
    systemprompt/           拼 system message
    contextmanager/         拼一次请求、按预算截断和压缩
    tools/                  工具契约与注册表
      shell/  mcp/          具体工具:命令、远程 MCP
      plan/  userinput/     update_plan、request_user_input
      subagent/             Agent 工具:派生子任务
      cron/                 定时任务工具与任务表
    skills/                 Agent Skills:技能目录、激活、读取资源
    memory/                 长期记忆:抽取、召回、编辑
  gateway/                  对外协议:WebSocket 上的 JSON-RPC 2.0
  repo/                     数据库(gorm):每张表的行结构体与读写方法
  pkg/                      通用组件,不知道 agent 的存在
    llm/  sandbox/  jsonrpc/  redact/
```

依赖只能往下指:

```
gateway
    ↓
agent(系统:会话、队列与 worker)
    ↓
runtime(一次迭代)
    ↓
systemprompt  contextmanager  permission  memory  tools/*  skills
    ↓
tools(工具契约)
    ↓
repo(数据库)
    ↓
pkg/*
```

**依赖都是真实的,不靠 interface 绕开。** 所有和数据库打交道的代码都在 `repo`:一张表一个文件(`workspaces.go`、`threads.go`、`turns.go`、`items.go`、`cron_jobs.go`、`memories.go`、`memory_revisions.go`、`memory_changes.go`、`memory_state.go`),里面是这张表的行结构体(gorm 据此建表)、它的读写方法和它身上那几个很轻的判断;`db.go` 是打开数据库和公用的小工具。行结构体就是业务直接用的类型,没有另一份业务模型,也没有转换函数:会话、turn、item 就是 `repo.ThreadRow`、`repo.TurnRow`、`repo.ItemRow`(带 JSON 标签,也就是接口返回给界面的样子),它们身上那几个很轻的判断(进行中的 turn、等待中的请求、子任务调用、会话状态)也定义在 repo 里。嵌套的字段用 gorm 的 JSON 序列化各存一列(turn 的模型,item 的审批、调用结果、冻结的命令等),用量是内嵌的三列。工作区、定时任务、长期记忆也一样:记忆就是 `repo.MemoryRow`(证据来源用 JSON 序列化存成一列),记忆状态就是 `repo.MemoryStatus`(几个计数加上 `memory_state` 那一行,上次的报告也是 JSON 序列化的一列)。业务规则(比如已准备的调用不能改、记忆的冲突与证据)留在各自包里。事务也不另设类型:`DB.Tx(func(tx *repo.DB) error)` 把同一个 `*repo.DB` 绑在事务上交给回调,所以每个读写方法在事务里外都能用,不会有"事务里一份、事务外一份"的重复方法。别的包需要的数据结构也尽量直接用现成的:contextmanager 拼的就是 `[]llm.ChatMessage`,技能目录工具直接返回 `skills.Skill`,定时任务工具直接用 `repo.CronJobRow`。不需要为了"分层"而定义 `Persistence`、`Store` 这类只有一个实现的接口。同理,runtime 直接持有 `*tools.Registry`、`*skills.Catalog`、`*memory.Manager` 和要执行的 `repo.TurnRow`,gateway 直接持有 `*agent.Agent`。runtime 写 item 用的是 agent 交给它的函数(写库并通知订阅者),所以它碰不到会话的存储。

整个库只有 2 个 interface,都是因为真的有多个实现:

- `tools.Provider` — 工具有很多种(shell、MCP、cron,还有应用自己的工具)。一个工具能做什么,写在它的 `tools.Spec` 上(权限匹配 `PermissionContent`、本地执行 `Local`、串行 `Serial`),不靠类型断言去猜它实现了哪些可选接口。
- `jsonrpc.Handler` — 通用的 JSON-RPC 连接回调 gateway。

设计上的三条规则:

1. **只能往下 import,同层之间互不依赖。** 所以 memory 不知道 cron,shell 不知道 mcp。
2. **runtime 只通过 `agent/tools` 使用工具**,不直接 import 任何具体工具。加一个工具或去掉一个工具都不用改 runtime。
3. **不随手加 interface。** 只有真的有多个实现时才用 interface。

项目还在快速迭代,目前没有测试,等实现稳定后再补。代码里不写注释,设计说明都在这份 README 里。

**agent 与 runtime 的几个关键行为**:

- **命令在哪里跑是 shell 自己的事。** agent 只知道会话属于哪个项目,把项目根目录放进 `tools.Caller.Root` 交给工具(system message 里也会写明项目根目录)。shell 每次准备命令时,用它和自己的配置现算命令能在哪些目录跑:有项目就在项目根目录下,没有就在配置的命令目录下。命令的冻结记录(`tools.Prepared`)绑定了目录和配置摘要(`SettingsDigest`);批准之后、执行之前如果设置或项目目录变了,这条命令不执行,模型收到"请重新调用"的结果,在同一个 turn 里重新发起,按新设置准备,需要的话重新审批。设置因此立即生效,不用开新 turn。
- **不读用户目录里的说明文件。** 不加载任何 `AGENTS.md` / `CLAUDE.md`。agent 自己的 prompt 由应用通过 `Agent.Prompt` 给出;留空时用 `systemprompt` 里写好的默认范本(以 `Agent.Name` 自称的个人助理:先弄清需求、能做就做、多步任务列计划、不越出项目目录、难以撤销的改动先说明、做完自查、照顾隐私)。用户的偏好由长期记忆自动学到,不需要用户自己写规则。
- **脱敏是双向的。** 发给模型前,把敏感值(手机号、邮箱、IP、密钥等)替换成稳定的占位符;模型的回答(流式文字、最终文字、工具调用参数)在被记录或交给工具之前,先把占位符还原成原始值。所以敏感值不会发给模型服务商,而会话记录、界面和工具看到的都是真实值。
- **只读工具并行执行。** 模型一次可以发起多个工具调用。runtime 按顺序逐个检查(参数、权限规则、审批);相邻的、已放行的只读调用会同时执行,其他调用单独执行,并等前面的调用完成。工具执行时不碰这次迭代的任何状态:进度行通过 channel 发回,结果作为返回值交回,所有记录都只由执行这次迭代的那一个 goroutine 按顺序写入,不依赖锁。驱动单一外部资源的只读工具(比如用户的浏览器)可以在 Spec 上标 `Serial`,不参与并行。
- **runtime 不认识任何具体工具。** 所有工具都来自注册表,runtime 只认工具结果的一种形态:`tools.Result.Await`,表示"这次调用在 turn 内答不了,由用户或另一个 agent 回答"。runtime 把它记在 item 上(调用保持进行中),继续处理同一回复里后面的调用;处理完后只要还有等待中的调用,这次迭代就以 `Wait` 结束。回答由 agent 写成那条调用的普通结果,turn 回到队列。审批不一样:等批准的调用会挡住后面的调用,因为后面的调用可能依赖它的效果。
- **模型跟着 turn 走。** 模型是一个值 `llm.Model{Name, Connection, Thinking}`,turn 开始时确定并存进 turn,之后这个 turn 的每次请求、重启后的恢复都用它。 用户可选的模型就是 `Agent.Models`,一组 `llm.Model`,每个连接一个,连接名(`Connection`)就是它的标识;不另设"模型档案"类型。`model/list` 返回这组模型和默认连接,`turn/start` 用 `connection` 指定这一轮用哪个,不指定就沿用这个会话上一轮的,新会话用 `Models` 的第一个。 模型(`llm.Model`,存进 turn 的值)和怎么连上它(`*llm.OpenAI`,客户端)是两回事:`Agent.Connections` 按连接名给出每个连接的客户端,`Start` 时检查 `Models` 里每个连接都有客户端;执行一个 turn 时,agent 按这个 turn 的 `Model.Connection` 取出客户端交给执行器。
- **没有子代理这个实体。** 派生出来的 turn 就是同一个 agent 在干活:同样的 prompt、工具、模型(沿用父 turn 的)和审批策略,任务内容来自父 turn 那次 `Agent` 调用的参数;每个派生的 turn 放在一个子会话里(有 `ParentThreadID`,会话列表里不显示),上下文只有这个任务。它不能再派生:system message 里写明了这一点,工具列表里没有 `Agent`(加了一条 deny 规则),就算真发起了 `Agent` 调用,agent 派生时也会拒绝。父 turn 等待时不在队列里;子 turn 结束时报告写回那次调用,全部到齐父 turn 才回到队列。
- **记忆在 turn 开始时冻结一次。** 第一次模型调用前,runtime 把本 turn 的 item 和 contextmanager 给的预算交给 `memory.TurnContext`,把它返回的 `memoryContext` item 记下来;之后的请求、审批后的恢复、重启都只读这个 item。"本 turn 是否已经选过、用哪些消息做查询、挑哪些、怎么控制长度、读失败怎么办"全在 memory 包里;runtime 只负责时机和记录。
- **上限只防失控。** 每个 turn 默认最多 200 次模型调用、400 次工具调用、连续 10 次失败(`agent/runtime` 里的常量,不对外配置)。Codex 没有上限,这些数值只是为了拦住陷入死循环的模型,不会打断正常的长任务。
- **不另记执行日志。** 执行过程的每一步都已经是 SQL 里的 item(模型调用的上下文统计和用量、工具调用的参数、审批、结果和错误、等待、压缩、强制收尾的原因),所以 runtime 和工具注册表不再往日志里重复写一遍;日志只记录 SQL 里没有的东西,也就是写库本身失败、后台循环出错、连接异常这类错误。
- **暂时没有缓存。** 所有数据只存在数据库里,每次读都查库。等业务代码稳定后,再按实际需要在合适的位置加缓存。
- **工具输出截断时保留头尾。** 输出超出预算时,保留开头三分之一和结尾三分之二,中间注明省略了多少字符;报错通常在结尾。

**各包职责**:

- `agent` — 整个系统:它自己的 prompt、模型、工具、审批策略、权限规则(审批策略和模型在 turn 创建时存进 turn;派生出来的子会话额外禁止再派生),它的会话和 turn 队列、4 个 worker。会话的存取与规则在 `store.go`:一个会话同时只有一个进行中的 turn、只写进行中 turn 的 item、item 不改类型和所属 turn、已准备的调用不能改;不存任何状态,每次读都查库,每次写都在一个事务里先读、检查规则、再写。写入都是 agent 的不导出方法,所以每次写入都伴随它的事件推送和调度。它只认工作区的一条规则:归档了的工作区不能开新会话、新 turn;项目根目录交给工具(`tools.Caller.Root`),目录能不能跑命令由 shell 在跑命令时判断。worker 把可前进的 turn 交给 runtime 跑一次迭代,按结果放回队列、等待或结束;用户的新消息、追加消息(`SteerTurn`)、批准、回答、中断、子代理的派生和报告、到期定时任务的发送、重启恢复都是它的方法。agent 有要告诉用户的事(流式文字、新的或完成的 item、等批准或回答、turn 开始和结束、会话状态),就调用应用给的 `OnEvent` 回调;它在 agent 持锁时被调用,必须很快返回、不能回调 agent。
- `agent/runtime` — ReAct 循环的一次迭代(`Runtime.Step`):先处理上次留下的调用,再请求一次模型并处理它的调用,返回 `Continue` / `Wait` / `Done`。`Runtime` 是一个 turn 的执行器,不是全局共享的对象:worker 每跑一次迭代就由 agent 新建一个,填上它用的部件(这个 turn 所在连接的模型客户端 `LLM`、工具注册表 `Registry`、技能目录、上下文管理、记忆)和这次要执行的东西:turn 本身(`Turn` 就是 `repo.TurnRow`,模型也从它上面取)、prompt、这个 turn 可用的工具(`Tools`)、权限检查器、调用者(会话、项目根目录)、写 item 的函数,以及这个 turn 已有的 item(`Items`)和之前各 turn 的 item(`History`,只用来拼上下文和查本会话的授权)。这次迭代里新产生的 item(模型的回答、工具调用和结果、进度、压缩)都由它记下并追加到 `Items`;迭代中的工作状态(system message、脱敏表、用量)是它的不导出字段,迭代结束就和它一起丢掉,迭代之间不保留状态。并行执行的工具只拿到注册表和调用者,碰不到执行器的状态。文件按内容分:`runtime.go`(Runtime、一次迭代的流程、停用工具后的强制收尾、记下 item)、`chat.go`(和模型的一次对话:拼请求、需要时先压缩、脱敏后发送再还原、检查回答、记下它请求的工具调用)、`stream.go`(流式文字)、`calls.go`(工具调用、调用次数与连续失败)。
- `agent/permission` — 授权判断:权限规则(deny / ask / allow,Claude Code 语法)、审批策略(`untrusted` / `on-request` / `never`)、用户之前"本会话都允许"的回答。`New` 在启动时对照工具目录检查规则,`Decide` 对一次调用按固定顺序给出 执行 / 问用户 / 拒绝:越权提权(`never` 下)→ deny → ask → allow → 本会话已允许 → 审批策略。工具只声明事实(只读、访问外部、规则匹配的内容、命令怎么跑),判断都在这里。
- `agent/contextmanager` — 把 system message 和对话拼成一次请求、按预算截断和压缩。预算是当前 turn 所用模型的输入上限(来自 `pkg/llm`);请求大小先按字符估算,再用同一模型上一次请求的真实 prompt tokens 校准。给出记忆、技能目录和已激活技能能占的预算(`MemoryBudget`、`SkillBudget`、`ArtifactBudget`)。
- `agent/systemprompt` — system message 的全部组成都在这里,按顺序拼:agent 自己的 prompt(`Agent.Prompt`,留空用默认范本 `AgentPrompt`)、子代理说明(子代理才有)、循环规则、执行环境、技能目录(在预算内整条列出,放不下的说明省略了几个)。每个 turn 开始时拼一次,turn 内不变,便于前缀缓存。
- `agent/tools` — 工具契约(`Provider`、`Spec`、`Result`、注解、本地命令的冻结记录 `Prepared`)与注册表;调用时通过 context 告诉工具进度回调和所属工作区。
- `agent/tools/shell` — `run_command` 工具:命令能在哪些目录跑(由调用者的项目根目录和配置现算,`dirs.go`)、命令参数、提权申请(`require_escalated`)、超时与黑名单,命令经 `pkg/sandbox` 执行;`shell.Config` 是用户可配的 exec 设置(带 JSON 字段名,`Normalize` 是唯一一份校验规则),agent 自己的数据目录和 skills 由它固定保护,应用不必也不能关掉。
- `agent/tools/mcp` — 远程 MCP 服务器的客户端,把它的工具接入注册表。
- `agent/tools/plan`、`agent/tools/userinput` — `update_plan`(计划就是调用参数)和 `request_user_input`(结果是 `Await` 给用户,回答成为调用结果)。
- `agent/tools/cron` — 定时任务的四个工具(创建、暂停/恢复、删除、列出),`*cron.Jobs` 本身就是工具提供者,读写 `repo` 的 cron_jobs 表:每个任务属于一个会话(`thread_id`,会话删除时一并删除),存着下次运行时间(`next_run_at`)。它只记录;到期的任务由 agent 发出(`agent/cron.go`,直接查 `DB.DueCronJobs`)。
- `agent/tools/subagent` — `Agent` 工具:模型用它派生子任务(简短说明 + 完整任务),结果是 `Await` 给另一个 turn;派生由 agent 完成。
- `agent/skills` — Agent Skills:`skills.New(dir)` 扫描、校验技能目录,得到的 `*skills.Catalog` 既是 runtime 把技能列进 prompt 的来源,本身也是提供列出、激活、读取资源三个工具的提供者。
- `agent/memory` — 长期记忆:后台抽取、用户编辑与遗忘(`Edit`、`Forget`,文字长度和凭据只在这里检查一次)、状态(`Status`);列出记忆没有包一层,gateway 直接读 `DB.Memories`;以及每个 turn 开始时给它的记忆(`TurnContext`):在调用方给的 token 预算内挑选、排序并渲染成 `memoryContext` item。

- `gateway` — WebSocket 上的 JSON-RPC 2.0 网关,方法名对齐 Codex app-server;另有 Ollupi 自己能力的方法(`memory/*`,以及直接调用工具的 `tool/call`)。应用自己的方法(比如读写它的配置文件)通过 `Options.Methods` 注册,不属于 Ollupi 的协议,也不能覆盖协议里的方法名。 应用用 `gateway.NewServer(agent, options)` 建好服务(长期记忆的接口直接用 `agent.Memory`),在 `agent.Start` 之前把 `agent.OnEvent` 接到 `server.Deliver`,再 `server.Serve(ctx, listener)`。`Deliver` 只是挑出关心这个会话的连接,把通知放进各自的发送队列,马上返回。 工作区的增删改查是界面的操作,也都在这里(`workspaces.go`):直接读写 `repo` 的工作区表;添加时把路径规范成真实目录(`sandbox.CanonicalDirectory`,shell 判断命令目录用的也是这一个),同一个目录再加一次复用已有的工作区,已归档的顺便恢复。
- `repo` — 数据库和数据模型,通过 gorm:打开 SQLite(WAL、外键、单连接),用行结构体建表(gorm 的 AutoMigrate,开发期没有 schema 版本,格式不兼容时重建数据库);gorm 不建模的部分(记忆变更队列的触发器)是原生 SQL(在 `memory_changes.go` 里),每次打开时重建;每张表的读写方法。不变的列由带条件的 upsert 保护:改了不该改的列,写入就失败。
- `pkg/llm` — 和模型对话的类型(`Model`、`ChatRequest`、`ChatResponse`)与 OpenAI 兼容 Chat Completions 客户端 `*llm.OpenAI`。模型客户端不是 interface:所有模型都走 OpenAI 兼容接口,测试时把客户端指向一个本地的假服务器即可。
- `pkg/sandbox` — macOS Seatbelt 沙箱:策略、profile、域名代理、进程监管、主机进程运行。
- `pkg/jsonrpc` — WebSocket 上的双向 JSON-RPC 2.0 连接。每个连接有自己的发送队列和写 goroutine:发出的响应、通知、请求先入队,按顺序写出;一个读得慢的客户端只会拖慢它自己。
- `pkg/redact` — 敏感值脱敏与还原。

## 与 Codex / Claude Code 的能力对照

完整的逐项能力对照见 [docs/agent-capabilities.md](docs/agent-capabilities.md)。
