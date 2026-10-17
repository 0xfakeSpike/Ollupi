# macOS 沙箱参考资料

这份清单整理自 2026-10-14 的调研，按主题分组，每条附一句"看什么"。标"已读"的我读过原文或源码；其余只看过搜索摘要，结论需要你自己核实。

## 建议阅读顺序

1. Codex 的实现（Daniel Vaughan 的文章）：先建立整体印象，看清它和 Ollupi 现在的结构有多接近。
2. sandbox-runtime 的 README 和 `macos-sandbox-utils.ts`：本次代理、拦截日志、"之后会被执行的文件"三项功能主要照它做。
3. Pillar Security 的 The Week of Sandbox Escapes：说明为什么要保护 `.vscode`、`.envrc` 这类文件。

## Codex（OpenAI）

- [Inside the Codex Sandbox: Platform-Specific Implementation on macOS, Linux and Windows](https://codex.danielvaughan.com/2026/04/08/codex-sandbox-platform-implementation/)（Daniel Vaughan，2026-04，10 月更新；已读）：统一的 `SandboxPolicy`，macOS 用 Seatbelt，Linux 用 Landlock + seccomp，Windows 用受限令牌 + ACL。`.git`、`.codex` 只读，`CODEX_SANDBOX` 环境变量，按启发式判断"是不是被沙箱拦了"再走升级流程。
- [Codex 官方：Agent approvals & security](https://developers.openai.com/codex/agent-approvals-security)：审批策略与沙箱模式的官方说明，网络代理开关的各种组合。
- [Codex 官方：Sandboxing](https://developers.openai.com/codex/concepts/sandboxing)：各平台要求，Linux 的 bubblewrap 回退方案。
- [DeepWiki：Codex Sandboxing Implementation](https://deepwiki.com/openai/codex/5.6-sandboxing-implementation)：按源码路径梳理，`seatbelt.rs`、`network-proxy` crate、`is_likely_sandbox_denied`。
- [Codex sandboxing technical analysis（rtzll 的 Gist）](https://gist.github.com/rtzll/8ec03ad8a4cca3ae43ce3db7eb7dcc09)：上面那篇文章的主要引用来源，细节更多。
- [openai/codex #10390](https://github.com/openai/codex/issues/10390)：`network_access = true` 被 Seatbelt 忽略的老 bug。也提醒一点：只开网络不开代理，等于外网完全放开。
- [openai/codex PR #12800](https://github.com/openai/codex/pull/12800)：zsh fork 绕过沙箱包装的修复。"每条执行路径都要包上沙箱"的反例。
- [Agent Safehouse：Codex CLI Sandbox Analysis](https://agent-safehouse.dev/docs/agent-investigations/codex)：第三方对 Codex 沙箱的审计。

## Claude Code / sandbox-runtime（Anthropic）

- [anthropics/sandbox-runtime](https://github.com/anthropics/sandbox-runtime)（已读 README 和 macOS 源码）：macOS 上用 sandbox-exec + 动态 profile，一个本地端口同时提供 HTTP 和 SOCKS5 代理按域名放行，profile 只放行这个代理端口。用 `(with message tag)` 给拦截规则打标签，再用 `log stream` 收集拦截记录并归到具体命令。`DANGEROUS_FILES` 和 `DANGEROUS_DIRECTORIES` 里列了 `.gitconfig`、`.vscode`、`.idea`、`.mcp.json`、shell 启动文件等。本次实现主要参照它。
  - 源码 [`src/sandbox/macos-sandbox-utils.ts`](https://github.com/anthropics/sandbox-runtime/blob/main/src/sandbox/macos-sandbox-utils.ts)：profile 的网络部分，含 IPv4 映射地址的坑和 Java 的 `preferIPv4Stack`；`startMacOSSandboxLogMonitor` 是拦截日志的实现。
  - 源码 [`src/sandbox/sandbox-utils.ts`](https://github.com/anthropics/sandbox-runtime/blob/main/src/sandbox/sandbox-utils.ts)：需要保护的文件清单（`DANGEROUS_FILES`），代理相关环境变量（`NO_PROXY`、`GRPC_PROXY`、`GIT_SSH_COMMAND`）的写法和原因。
- [claude-code #77045](https://github.com/anthropics/claude-code/issues/77045)：macOS 上 `allowedDomains` 没有生效的 bug 报告，代理对不在白名单的主机也放行了 CONNECT。自己写代理时要拿它当测试用例。
- [Sandboxing Claude Code on macOS: What I Actually Found](https://www.infralovers.com/blog/2026-02-15-sandboxing-claude-code-macos/)：使用者的实测记录。
- [Claude Code 官方：沙箱](https://code.claude.com/docs/en/sandboxing)

## 其他 macOS 沙箱方案

- **基于 Seatbelt**
  - Agent Safehouse（[对 Codex 的分析](https://agent-safehouse.dev/docs/agent-investigations/codex)）：一个 shell 脚本，默认全拒绝，整理了很全的敏感路径清单。
  - [bkircher/seatbelt](https://github.com/bkircher/seatbelt)：sandbox-exec 的轻量包装，README 讲清楚了"Seatbelt 不能按域名过滤网络"。
  - [holtwick/bx-mac](https://github.com/holtwick/bx-mac)：默认放行、只拦敏感路径，不管网络，是比较宽松的一类。
- **另开一个用户账号**
  - [AkihiroSuda/alcless](https://github.com/AkihiroSuda/alcless)：作者是 Akihiro Suda，用另一个用户加 rsync 实现，改动同步回原目录前要你确认。另有作者的[介绍文章](https://medium.com/nttlabs/alcoholless-a-lightweight-security-sandbox-for-macos-programs-homebrew-ai-agents-etc-ccf0d1927301)。
  - [webcoyote/sandvault](https://github.com/webcoyote/sandvault)：另开用户再叠加 sandbox-exec。
- **虚拟机**（隔离最强，但里面是 Linux，用不了你的 macOS 工具链）
  - [A Deep Dive into apple/container](https://labhub.hopto.org/blog/virtualization/2026-06-12-apple-container-lightweight-vm?lang=en)：Apple container，每个容器一个轻量虚拟机。
  - [Sandboxing AI coding agents with Lima](https://bogoyavlensky.com/blog/sandboxing-ai-coding-agents-with-lima/)
  - [sugarforever/apple-sandbox](https://github.com/sugarforever/apple-sandbox)：在 Apple container 里跑 codex exec-server。
- **汇总**
  - [restyler/awesome-sandbox](https://github.com/restyler/awesome-sandbox)
  - [A deep dive on agent sandboxes（Pierce Freeman）](https://pierce.dev/notes/a-deep-dive-on-agent-sandboxes)
  - [AI Agent Sandboxes: 30 Providers Compared](https://engine.build/lab/agent-sandboxes)

## 安全研究：2026 年 7 月的沙箱逃逸

- [Pillar Security：The Week of Sandbox Escapes](https://www.pillar.security/blog/the-week-of-sandbox-escapes)（2026-07-20，原文未读，以下来自摘要和报道）：Cursor、Codex CLI、Gemini CLI、Antigravity 都被逃逸了。几乎所有案例都没有攻破沙箱本身，而是往磁盘写一个"之后会被沙箱外的可信程序执行"的东西：hook 配置、git config、venv 里的解释器、task 定义；另外几家还被 Docker socket 打穿了。原文结论是：如果 agent 能写其他系统将来要读的输入，它从一开始就没被沙箱住。
- [BleepingComputer 报道](https://www.bleepingcomputer.com/news/security/cursor-codex-gemini-cli-antigravity-hit-by-sandbox-escapes/)
- [Cloud Security Alliance Research Note](https://labs.cloudsecurityalliance.org/research/csa-research-note-ai-coding-agent-sandbox-escapes-20260722-c/)：把这类问题叫作"信任交接漏洞"（trust handoff flaw）。
- [Sandbox Your Local AI Agents on macOS: Agent Safehouse and the Deny-First Fix](https://runaihome.com/blog/sandbox-local-ai-agents-macos-agent-safehouse-2026/)

## Seatbelt 规则语言的坑（unix socket 路径过滤）

- [mokmok-dev/agent PR #2](https://github.com/mokmok-dev/agent/pull/2)：实测 `literal`/`subpath` 匹配不到 unix socket 的 connect，`regex` 可以。
- [drawmeanelephant/gostalgia PR #103](https://github.com/drawmeanelephant/gostalgia/pull/103)：同样的结论，他们改成了预先连好的 socketpair。
- [prismatic-koi/nixos-config #3066](https://github.com/prismatic-koi/nixos-config/issues/3066)：主张默认拒绝全部 unix socket，只放行 mDNSResponder 等少数几个。
- [brave/bravebot #980](https://github.com/brave/bravebot/issues/980)：经 Docker socket 挂载 home 目录的逃逸。
- [A quick glance at macOS' sandbox-exec（Julio Merino）](https://jmmv.dev/2019/11/macos-sandbox-exec.html)：入门介绍，Bazel 的 profile 写法。

注意：sandbox-runtime 的源码里对 unix socket 用的是 `subpath`，和上面两份实测结论相反。Ollupi 用的是 `regex`，到 Mac 上跑 `TestSeatbeltUnixSocketsStayClosedWithTheNetworkOn` 就能确认哪种对。
