# agy-pro

> Unofficial native zero-gateway multi-account pool & auto-failover wrapper for Antigravity CLI (`agy`).

纯净、零中转网关、单文件二进制的原生 Antigravity CLI (`agy`) 多账号号池管理工具（Go 实现）。

## 核心特性

- **极速单文件二进制**：Go 静态编译单二进制落盘至 `~/.local/bin/agy-pro`，本地命令（`status`/`use`/`next`）`< 5ms` 瞬出，常驻内存仅 ~4MB。
- **零外部代理依赖**：独立号池仓库位于 `~/.local/state/agy-pro/`（`0700`/`0600` 权限），内置 `login`/`import`/`remove`/`order`，彻底独立运行。
- **100% 零入侵原生直连**：直接拉起官方 `agy` 二进制，不强行锁死模型、不覆盖 `--settings`，完全尊重你的 `agy` 原生配置与对话内 `/model` 切换；保留真实 `$HOME`，官方升级新模型或新交互特性时自动兼容、永不失配。
- **严格 `drain` 切号**：单号死磕直到 5h（或 7d）耗尽，顺延切到下个账号且不回跳。
- **零误判撞墙检测**：绝不盲扫 PTY 终端屏幕文本；仅监听 `agy` 后端结构化 429 日志并在切号前强制实时核验剩余额度（`<= 0.5%`），杜绝历史会话回放或临时 429 误杀健康账号。
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
agy-pro remove <关键词>          # 移除账号
agy-pro clear-cooldown          # 清空本地冷却与耗尽缓存
agy-pro doctor                  # 环境与号池体检
```

## Disclaimer

`agy-pro` is an independent, unofficial CLI wrapper and is not affiliated with or endorsed by Google. It invokes your locally installed official `agy` binary directly without modifying its protocol or proxying API requests.
