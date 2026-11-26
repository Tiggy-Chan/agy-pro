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
- **默认 80% Auto-compact + 首次启动自动配置**：内置 `Auto-compact = 80%` 默认配置（通过官方 `AGY_CLI_AUTO_COMPACT_OVERRIDE` 生效），首次启动时自动写入状态并同步配置 Shell 环境使原生 `agy` 一并生效；支持在 `agy-pro ui` 中按 `[` / `]` 实时步进微调（`a` 一键重置/开关）或通过 `agy-pro config auto-compact` / `--auto-compact` 自行修改。
- **零 PTY 中间层（零渲染干涉）**：交互会话采用原生 TTY 直通（`stdin/stdout/stderr` 直接继承，不套二层 PTY 转发），渲染与快捷键行为和官方原生 `agy` 100% 一致；所有后台额度查询严格 `Setsid + Stdin=/dev/null` 脱离 `/dev/tty`；切号/强制终止时由 `TerminalGuard` 无条件恢复 `termios` 并复位 Kitty Keyboard Protocol (`\x1b[<u`) / `modifyOtherKeys` / Bracketed Paste / Mouse Tracking。

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

agy-pro ui                                     # 交互式号池仪表盘（选号启动、自动/手动刷新、J/K 调序、+/- 水位线、[/]/a 压缩、l/d 登录移除）
agy-pro status [-r] [--active] [--reset]       # <5ms 双池看板（-r 联网刷新、--active 仅刷主号、--reset 清冷却与耗尽缓存、--json）
agy-pro resume [会话ID] [agy原生参数...]        # 交互式会话选择器（当前/全部目录 + 实时搜索 + Enter/Ctrl+N 恢复；传 ID 时直连恢复）
agy-pro next [agy原生参数...]                  # 切到下个可用账号；带参数时直接按新号启动 agy（兼容 --dangerously-skip-permissions / -c / --conversation）
agy-pro use <关键词> [agy原生参数...]           # 指定当前主号；带参数时直接按该号启动 agy
agy-pro auth <login | import <路径> | rm <号>>  # 账号凭据管理（调用官方 agy acp --login 登录、导入 token 文件/目录、移除账号）
agy-pro config [reserve <0-50> | auto-compact <80%|800K|off>]  # 查看/设置低余量切号水位线与默认 Auto-compact 阈值
agy-pro doctor                                 # 运行环境隔离与号池凭据健康体检
```

## Disclaimer

`agy-pro` is an independent, unofficial CLI wrapper and is not affiliated with or endorsed by Google. It invokes your locally installed official `agy` binary directly without modifying its protocol or proxying API requests.
