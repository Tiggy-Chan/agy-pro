<p align="center">
  <img src="docs/assets/logo.svg" width="112" height="112" alt="agy-pro logo" />
</p>

<h1 align="center">agy-pro</h1>

<p align="center">
  <strong>官方 Antigravity CLI（<code>agy</code>）的零中转网关多账号池、自动续跑与实时监控增强工具。</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square" alt="Go 1.26+" />
  <img src="https://img.shields.io/badge/Platform-Linux%20%7C%20macOS-1F2328?style=flat-square" alt="Linux & macOS" />
  <img src="https://img.shields.io/badge/Architecture-Zero%20Proxy%20%7C%20Zero%20PTY-2E7D32?style=flat-square" alt="零中转零PTY" />
  <img src="https://img.shields.io/badge/i18n-EN%20%7C%20%E4%B8%AD%E6%96%87-5B6678?style=flat-square" alt="中英双语" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Non--Commercial-3A6FE0?style=flat-square" alt="非商业自用许可" /></a>
</p>

<p align="center">
  <a href="README.md">English</a> · <a href="README_CN.md"><strong>简体中文</strong></a>
</p>

**agy-pro** 是基于 Go 实现的单文件二进制、零中转网关、零入侵原生 Antigravity CLI（`agy`）多账号与会话增强工具。它直接把真实 TTY 交给本机官方 `agy`——不架代理网关、不套二层 PTY、不强锁模型，默认不改任何用户 dotfiles。

---

## 核心特性

- **极速单文件二进制（零 CGO）**：Go 纯静态编译落盘至 `~/.local/bin/agy-pro`（支持 Linux / macOS `amd64`/`arm64`），本地命令 `< 5ms` 瞬出，常驻内存仅 ~4 MB。
- **零中转直连 + 独立号池**：独立凭据仓位于 `~/.local/state/agy-pro/`（`0700`/`0600`），通过官方 `AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1` 实现文件凭据隔离（零 `SSH_CONNECTION` 伪造，不碰系统钥匙串），保留真实 `$HOME`、原生模型配置与 `/model` 切换。
- **亚秒级直连查额度 + ~1.5s 撞墙秒切续跑**：
  - 严格 `drain` 轮换（单号用尽再顺延，绝不来回横跳）；支持低余量提前切号水位线（`agy-pro config reserve 10` 或 `--reserve 10`）。
  - 优先通过 Go 标准库共享 HTTP/2 gRPC 直连官方 `FetchQuotaStatus`（4 账号并发 **~0.8s 秒出**），access token 过期或接口变更时无缝回退官方 `agy --print /usage`。
  - 撞上后端致命 429 额度墙时立刻秒切备号并 `--resume` 续跑，独立后台进程异步核验精准 5h/7d 重置时间（若为临时 429 自动解冻）。
- **三合一交互式终端面板（`agy-pro ui`）**：
  - **号池（`1`）**：双池额度条、按 `J`/`K` 调序、`+/-` 调水位线、`[/]/a` 调压缩阈值、`l`/`d` 登录与移除、`g` 实时切语言。
  - **当前打开（`2`）**：基于官方 `~/.gemini/antigravity-cli/presence/*.lock` 的非阻塞内核 `flock` 探测，`< 0.5ms` 精准识别本机当前真实打开的所有 `agy` 终端（同时兼容 `agy-pro` 与裸 `agy`，附带实时账号与 TTY 号）；按 `a` 可切换「仅打开 / 含历史」（自动隐藏 0 步空壳历史并将打开中窗口置顶）。
  - **用量统计（`3`）**：只读并发扫描本地 `conversations/*.db`（关联单次调用精确时间戳并归并子代理），按**今日 / 近 7 天 / 全部**统计精确 **总计 / 未缓存输入 / 缓存读 / 命中率 / 输出 / 思考占比**，按模型（含 `TOTAL` 列与尾项汇总）及 Top 消耗会话排行，`Enter`/`Ctrl+N` 直接续聊，按 `r` 强制清缓存重扫。
- **交互式会话选择器（`agy-pro resume`）**：纯 Go 只读检索 `conversation_summaries.db`（对标 `claude --resume`），自动隐藏 0 步空占位记录，默认当前目录、`Tab` 切全部目录、直接打字实时搜索、`Enter` 当前号恢复、`Ctrl+N` 下个号恢复。
- **实时终端标签页标题 & 长任务桌面通知**：
  - 无入侵通过原子 OSC 0 同步会话名（实时响应对话内 `/rename`）与状态（`⠋` 工作中 / `✦` 就绪 / `↻` 切号中），退出自动还原原标签页标题（`AGY_PRO_NO_TITLE=1` 可关闭）。
  - 连续运行 ≥ 30 秒的任务完成或触发 429 自动切号时发送轻量系统桌面通知（Linux `notify-send` / macOS `osascript`，无头 SSH 自动静默，`agy-pro config notify on|off` 可控）。
- **默认 90% Auto-compact（零 dotfiles 副作用）+ 中文 OSC-8 重影防护**：
  - 默认仅向子进程注入 `AGY_CLI_AUTO_COMPACT_OVERRIDE=90%`，绝不擅自改写 `~/.bashrc`/`~/.zshrc`（仅在显式追加 `--sync-shell` 时同步）。
  - 默认注入 `AGY_CLI_FORCE_OSC8=0`（可 `AGY_CLI_FORCE_OSC8=1` 覆盖）规避原生 `agy` 中文 OSC-8 超链接 `0x9C`/`0x8D` 光标上跳重影；`launch.lock` 护住多终端并发启动握手；`TerminalGuard` 切号时无条件复位 `termios` 与 Kitty 键盘协议。
- **外置化多语言包（中/英自动适配）**：默认按系统语言自动选择（中文系统显示中文，其余统一显示英文），支持 `agy-pro config lang <auto|en|zh>`、`AGY_PRO_LANG` 环境变量或在 `agy-pro ui` 中按 `g` 一键切换。所有文案均外置在 JSON 语言包（`internal/i18n/locales/*.json`，并支持在 `~/.local/state/agy-pro/locales/` 投放扩展语言包）。

---

## 安装与快速上手

**前置条件**：本机已安装官方 `agy` CLI、`Go 1.26+`，且 `~/.local/bin` 已加入 `PATH`。

```bash
git clone https://github.com/Tiggy-Chan/agy-pro.git
cd agy-pro
make test      # 运行全部单元测试与真实 PTY 终端安全测试
make install   # 编译并安装单二进制至 ~/.local/bin/agy-pro
```

安装完成后登录 2 个及以上 Google 账号即可开跑：

```bash
agy-pro auth login   # 调用官方 `agy acp --login` 登录至独立号池（可多次添加多个账号）
agy-pro ui           # 打开三视图面板（号池 / 当前打开 / 用量统计）
agy-pro              # 直接启动原生 agy，撞 5h/7d 墙自动秒切续跑
```

---

## 核心命令（8 个）

```bash
agy-pro                                        # 秒开原生 agy（直连 Google + 撞 5h/7d 墙自动秒切续跑）
agy-pro -- <agy原生参数...>                     # POSIX 显式透传全部原生参数给 agy（如 --dangerously-skip-permissions）

agy-pro ui                                     # 三视图交互面板（Tab/1-3 切换 号池 / 当前打开 / 用量统计，g 切语言）
agy-pro status [-r] [--active] [--reset]       # <5ms 双池看板（-r 联网刷新、--active 仅刷主号、--reset 清冷却、--json）
agy-pro resume [会话ID] [agy原生参数...]        # 交互式会话选择器（当前/全部目录 + 实时搜索 + Enter/Ctrl+N 恢复）
agy-pro next [agy原生参数...]                  # 切到下个可用账号；带参数时直接按新号启动 agy
agy-pro use <关键词> [agy原生参数...]           # 指定当前主号；带参数时直接按该号启动 agy
agy-pro auth <login | import <路径> | rm <号>>  # 账号凭据管理（调用官方 agy acp --login 登录、导入、移除）
agy-pro config [reserve <0-50> | auto-compact <90%|800K|off> [--sync-shell] | notify <on|off> | lang <auto|en|zh>]
agy-pro doctor                                 # 运行环境隔离、上游数据库契约与号池凭据健康体检
```

<details>
<summary><strong>常用启动选项与环境变量速查</strong></summary>

| 选项 / 环境变量 | 说明 |
| --- | --- |
| `--account <关键词>` | 本次临时指定某个账号（匹配邮箱前缀或子串） |
| `--title <名称>` | 自定义终端标签页标题（对话内 `/rename` 后仍实时同步） |
| `--reserve <0-50>` | 本次覆盖低余量提前切号水位线 |
| `--auto-compact <值>` | 本次覆盖 Auto-compact 阈值（如 `90%`、`800K`、`700000`、`off`） |
| `--refresh` | 启动前先联网刷新当前主号实时额度 |
| `--no-auto-switch` | 本次关闭撞 5h/7d 墙自动秒切续跑 |
| `AGY_PRO_LANG` | 覆写界面语言（`en` / `zh`） |
| `AGY_PRO_NO_TITLE=1` | 关闭 OSC 0 终端标签页实时标题同步 |
| `AGY_PRO_STATE_DIR` | 指定独立号池与状态目录（默认 `~/.local/state/agy-pro`） |
| `AGY_CLI_FORCE_OSC8=1` | 重新开启原生 `agy` 的 OSC-8 超链接（默认关闭以防中文路径重影） |

</details>

---

## 项目寄语

> 读完 `agy-pro` 的源码，最打动人的不是炫技，而是克制。它拒绝中转代理与二层 PTY 包装，将真 TTY 原样交还官方 `agy`，自己退居幕后追日志、只读查 SQLite，以 `flock` 与原子写护住状态；查额度手写轻量 Protobuf 线解析直连 gRPC，却仍留好官方 CLI 兜底。更见功力的是踩过真坑才有的细节：防中文 UTF-8 误触 C1 控制符的 OSC-8 重影规避、切号时连 Kitty 键盘协议一并复位的 `TerminalGuard`，以及为毫秒启动关闭的 OSC 11 探测。
>
> 愿 `agy-pro` 始终守住这副“隐形外骨骼”的分寸，抵抗功能膨胀，让原生 `agy` 永远当主角。也要保持清醒：日志正则、库表字段与线协议编号终究是贴着上游的脆弱接缝，唯有把优雅降级与边界测试打牢，才能在上游演进中长久保持小而锋利、稳而不扰的生命力。

---

## 免责声明

`agy-pro` 是一个独立的非官方 CLI 增强封装工具，与 Google 无任何关联或背书。它直接调用用户本机已安装的官方 `agy` 二进制文件，不修改其二进制内容，也不代理或转发任何 API 请求。
