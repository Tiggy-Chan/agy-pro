# agy-pro

> Unofficial native zero-gateway multi-account pool & auto-failover wrapper for Antigravity CLI (`agy`).

纯净、零中转网关、单文件二进制的原生 Antigravity CLI (`agy`) 多账号号池与会话增强工具（Go 实现）。

## 核心特性

- **极速单文件二进制**：Go 纯静态编译（零 CGO）单二进制落盘至 `~/.local/bin/agy-pro`，本地命令（`status`/`use`/`next`）`< 5ms` 瞬出，常驻内存仅 ~4MB。
- **零外部代理依赖**：独立号池仓库位于 `~/.local/state/agy-pro/`（`0700`/`0600` 权限），内置 `auth login/import/rm`，彻底独立运行。
- **100% 零入侵原生直连**：直接拉起官方 `agy` 二进制，通过官方 `AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1` 实现文件凭据隔离（零 `SSH_CONNECTION` 伪造），不强行锁死模型、不覆盖 `--settings`，完全尊重你的 `agy` 原生配置与对话内 `/model` 切换；保留真实 `$HOME`，官方升级新模型或新交互特性时自动兼容。
- **交互式会话选择器（`agy-pro resume`）**：只读检索原生 `agy` 会话库（`conversation_summaries.db`），默认展示当前工作目录的历史会话，`Tab` 一键切换 `当前目录 / 全部目录`，直接打字实时模糊搜索（标题/预览/目录/ID），`Enter` 用当前号恢复、`Ctrl+N` 切到下个号恢复，并透传 `--dangerously-skip-permissions` 等原生参数。
- **严格 `drain` 切号**：单号死磕直到 5h（或 7d）耗尽，顺延切到下个账号且不回跳。
- **极速异步秒切（~1.5s）+ 亚秒级直连双池核验**：撞上 `agy` 后端致命 429 额度墙时立刻秒切到下一个可用账号并 `--resume` 续跑；双池额度查询（`status -r` / `ui` / 后台核验）优先通过 Go 原生 HTTP/2 gRPC 直连官方 `FetchQuotaStatus` 接口（**~0.5s 并发秒出**，不冷启动 `agy` 进程），仅在 access token 过期或接口变更时自动回退至官方 `agy --print /usage` 完成刷新续签。
- **低余量水位线提前切号**：支持 `agy-pro config reserve 10`（或单次 `--reserve 10`），新会话启动前若主号剩余额度低于水位线，直接提前顺滑切至满血备号，大幅减少长任务中途撞墙概率。
- **默认 80% Auto-compact（零 dotfiles 副作用）**：内置 `Auto-compact = 80%` 默认配置（通过官方 `AGY_CLI_AUTO_COMPACT_OVERRIDE` 仅向子进程注入，默认**绝不修改**你的 `~/.bashrc`/`~/.zshrc`）；支持在 `agy-pro ui` 中按 `[` / `]` 实时步进微调（`a` 一键开关）或通过 `agy-pro config auto-compact <值>` 修改（如需让原生 `agy` 也一并生效，可显式加 `--sync-shell`）。
- **实时终端标签页标题、运行状态与长任务桌面通知**：无入侵通过 OSC 0 自动把会话名与实时运行状态同步到终端标签页——工作中旋转 `⠋ <会话名>`、就绪/完成显示 `✦ <会话名>`、切号中显示 `↻ <会话名>`；支持对话内官方 `/rename <名字>` 实时改名同步（或 `--title <名称>`）；连续运行 ≥ 30 秒的任务完成或触发 429 自动切号时发送轻量系统桌面通知（`agy-pro config notify on|off` 可控，无头 SSH 自动静默）。
- **零 PTY 中间层 + 多终端并发启动锁 + 中文 OSC-8 重影防护**：交互会话采用原生 TTY 直通，并引入毫秒级 `launch.lock` 保护多标签页并发启动时的凭据握手窗口；默认传入官方 `AGY_CLI_FORCE_OSC8=0`（可 `AGY_CLI_FORCE_OSC8=1` 覆盖）规避原生 `agy` 中文 OSC-8 超链接 `0x9C`/`0x8D` 光标上跳重影；切号/终止时由 `TerminalGuard` 无条件复位 `termios` 与 Kitty 键盘协议。

## 安装与编译

```bash
git clone https://github.com/Tiggy-Chan/agy-pro.git
cd agy-pro
make test      # 运行全部单元测试与真实 PTY 终端安全测试
make install   # 编译并安装单二进制至 ~/.local/bin/agy-pro
```

## 核心命令（8 个）

```bash
agy-pro                                        # 秒开原生 agy（直连 Google + 撞 5h/7d 墙自动秒切续跑）
agy-pro -- <agy原生参数...>                     # POSIX 显式透传全部原生参数给 agy（如 --dangerously-skip-permissions）

agy-pro ui                                     # 交互式号池与活跃会话仪表盘（Tab 切号池/活跃会话监控、选号启动、刷新、调序、调水位线/压缩、登录移除）
agy-pro status [-r] [--active] [--reset]       # <5ms 双池看板（-r 联网刷新、--active 仅刷主号、--reset 清冷却与耗尽缓存、--json）
agy-pro resume [会话ID] [agy原生参数...]        # 交互式会话选择器（当前/全部目录 + 实时搜索 + Enter/Ctrl+N 恢复；传 ID 时直连恢复）
agy-pro next [agy原生参数...]                  # 切到下个可用账号；带参数时直接按新号启动 agy（兼容 --dangerously-skip-permissions / -c / --conversation）
agy-pro use <关键词> [agy原生参数...]           # 指定当前主号；带参数时直接按该号启动 agy
agy-pro auth <login | import <路径> | rm <号>>  # 账号凭据管理（调用官方 agy acp --login 登录、导入 token 文件/目录、移除账号）
agy-pro config [reserve <0-50> | auto-compact <值|off> [--sync-shell] | notify <on|off>]
agy-pro doctor                                 # 运行环境隔离、上游数据库契约与号池凭据健康体检
```

## 项目寄语

> 读完 `agy-pro` 的源码，最打动人的不是炫技，而是克制。它拒绝中转代理与二层 PTY 包装，将真 TTY 原样交还官方 `agy`，自己退居幕后追日志、只读查 SQLite，以 `flock` 与原子写护住状态；查额度手写轻量 Protobuf 线解析直连 gRPC，却仍留好官方 CLI 兜底。更见功力的是踩过真坑才有的细节：防中文 UTF-8 误触 C1 控制符的 OSC-8 重影规避、切号时连 Kitty 键盘协议一并复位的 `TerminalGuard`，以及为毫秒启动关闭的 OSC 11 探测。
>
> 愿 `agy-pro` 始终守住这副“隐形外骨骼”的分寸，抵抗功能膨胀，让原生 `agy` 永远当主角。也要保持清醒：日志正则、库表字段与线协议编号终究是贴着上游的脆弱接缝，唯有把优雅降级与边界测试打牢，才能在上游演进中长久保持小而锋利、稳而不扰的生命力。

## Disclaimer

`agy-pro` is an independent, unofficial CLI wrapper and is not affiliated with or endorsed by Google. It invokes your locally installed official `agy` binary directly without modifying its protocol or proxying API requests.
