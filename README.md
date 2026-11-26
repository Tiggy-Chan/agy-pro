# agy-pro

> Unofficial native zero-gateway multi-account pool & auto-failover wrapper for Antigravity CLI (`agy`).

纯净、零中转网关、单文件二进制的原生 Antigravity CLI (`agy`) 多账号号池管理工具（Go 实现）。

## 核心特性

- **极速单文件二进制**：Go 静态编译单二进制落盘至 `~/.local/bin/agy-pro`，本地命令（`status`/`use`/`next`）`< 5ms` 瞬出，常驻内存仅 ~4MB。
- **零外部代理依赖**：独立号池仓库位于 `~/.local/state/agy-pro/`（`0700`/`0600` 权限），内置 `login`/`import`/`remove`/`order`，彻底独立运行。
- **100% 零入侵原生直连**：直接拉起官方 `agy` 二进制，不强行锁死模型、不覆盖 `--settings`，完全尊重你的 `agy` 原生配置与对话内 `/model` 切换；保留真实 `$HOME`，官方升级新模型或新交互特性时自动兼容、永不失配。
- **严格 `drain` 切号**：单号死磕直到 5h（或 7d）耗尽，顺延切到下个账号且不回跳。
- **极速异步秒切（~1.5s）+ 双池核验**：撞上 `agy` 后端致命 429 额度墙时，不再阻塞等待 35 秒查额度，而是立刻秒切到下一个可用账号并 `--resume` 续跑，同时由独立后台进程异步核验旧号 Gemini + Claude/GPT 双池的精准 5h/7d 重置时间写回状态（若确认仅为临时 429 则自动解冻）；并支持后台静默预热备号登录态。
- **低余量水位线提前切号**：支持 `agy-pro config reserve 10`（或单次 `--reserve 10`），新会话启动前若主号剩余额度低于水位线，直接提前顺滑切至满血备号，大幅减少长任务中途撞墙概率。
- **默认 80% Auto-compact + 首次启动自动配置**：内置 `Auto-compact = 80%` 默认配置（通过官方 `AGY_CLI_AUTO_COMPACT_OVERRIDE` 生效），首次启动时自动写入状态并同步配置 Shell 环境使原生 `agy` 一并生效；支持在 `agy-pro ui` 中按 `[` / `]` 实时步进微调（`a` 一键重置/开关）或通过 `agy-pro config auto-compact` / `--auto-compact` 自行修改。
- **零 PTY 中间层（零渲染干涉）**：交互会话采用原生 TTY 直通（`stdin/stdout/stderr` 直接继承，不套二层 PTY 转发），渲染与快捷键行为和官方原生 `agy` 100% 一致；所有后台额度查询严格 `Setsid + Stdin=/dev/null` 脱离 `/dev/tty`；切号/强制终止时由 `TerminalGuard` 无条件恢复 `termios` 并复位 Kitty Keyboard Protocol (`\x1b[<u`) / `modifyOtherKeys` / Bracketed Paste / Mouse Tracking。
- **自动续跑**：撞 5h 墙确认后自动切号，静默预刷新过期 token，并用原生 `agy --resume <id> --prompt-interactive` 续接完整上下文。

## 安装与编译

```bash
git clone https://github.com/Tiggy-Chan/agy-pro.git
cd agy-pro
make test      # 运行全部单元测试与真实 PTY 终端安全测试
make install   # 编译并安装单二进制至 ~/.local/bin/agy-pro
```

## 常用命令

```bash
agy-pro                         # 秒开原生 agy（尊重你的默认模型配置 + 撞 5h/7d 墙自动切号续跑）
agy-pro ui                      # 打开交互式 BubbleTea 仪表盘（选号启动、实时刷新、调序、调水位线）
agy-pro --dangerously-skip-permissions -r <ID> # 直接兼容并透传 agy 原生参数
agy-pro status                  # <5ms 秒显看板（自适应 80 列终端，中文精准对齐）
agy-pro status -r               # 联网刷新全部账号实时 5h/7d 额度（带逐号进度条）
agy-pro status -r --active      # 仅联网刷新当前主号额度
agy-pro login                   # 调用官方 `agy acp --login` 登录新 Google 账号进独立号池
agy-pro import <文件或目录>      # 导入已有 antigravity-oauth-token 到独立号池
agy-pro use <关键词>             # 手动切换当前主号（如 agy-pro use alice）
agy-pro next [--resume]         # 切到下个可用账号（加 --resume 续聊上次会话）
agy-pro resume [会话ID]         # 续聊当前目录最近一次（或指定）会话
agy-pro order <号1> <号2> ...   # 自定义 drain 轮换顺序
agy-pro config [reserve <0-50>]       # 设置低余量提前切号水位线（如 reserve 10）
agy-pro config auto-compact <值|off>  # 设置默认 Auto-compact（默认 80%，支持 %/K/M/off）
agy-pro remove <关键词>          # 移除账号
agy-pro clear-cooldown          # 清空本地冷却与耗尽缓存
agy-pro doctor                  # 环境与号池体检
```

## Disclaimer

`agy-pro` is an independent, unofficial CLI wrapper and is not affiliated with or endorsed by Google. It invokes your locally installed official `agy` binary directly without modifying its protocol or proxying API requests.
