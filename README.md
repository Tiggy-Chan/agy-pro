<p align="center">
  <img src="docs/assets/logo.svg" width="112" height="112" alt="agy-pro logo" />
</p>

<h1 align="center">agy-pro</h1>

<p align="center">
  <strong>Zero-gateway multi-account pool, auto-failover &amp; live session monitor for Antigravity CLI (<code>agy</code>).</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square" alt="Go 1.26+" />
  <img src="https://img.shields.io/badge/Platform-Linux%20%7C%20macOS-1F2328?style=flat-square" alt="Linux and macOS" />
  <img src="https://img.shields.io/badge/Architecture-Zero%20Proxy%20%7C%20Zero%20PTY-2E7D32?style=flat-square" alt="Zero Proxy Zero PTY" />
  <img src="https://img.shields.io/badge/i18n-EN%20%7C%20%E4%B8%AD%E6%96%87-5B6678?style=flat-square" alt="English and Chinese" />
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Non--Commercial-3A6FE0?style=flat-square" alt="License Non-Commercial" /></a>
</p>

<p align="center">
  <a href="README.md"><strong>English</strong></a> · <a href="README_CN.md">简体中文</a>
</p>

**agy-pro** is a pure-Go, single-binary, zero-intrusion native wrapper for Google's Antigravity CLI (`agy`). It invokes your locally installed `agy` binary directly on the real TTY — no API proxy gateway, no second-layer PTY wrapper, no forced model overrides, and zero dotfiles mutations by default.

---

## Key Features

- **Single Static Binary (Zero CGO)**: Compiles cleanly to `~/.local/bin/agy-pro` on Linux and macOS (`amd64` / `arm64`). Local commands (`status`, `use`, `next`) finish in `< 5ms` with ~4 MB RSS.
- **Zero-Proxy Native Execution**: Isolated account vault under `~/.local/state/agy-pro/` (`0700`/`0600`) using official `AGY_CLI_FORCE_FILE_TOKEN_STORAGE=1` (no system Keychain writes, no `SSH_CONNECTION` spoofing). Preserves your real `$HOME`, native model settings, and `/model` switching.
- **Sub-Second Direct Quota Probing + ~1.5s Auto-Failover**:
  - Strict **drain** rotation (exhausts one account's 5h/7d pool before advancing; never bounces back).
  - Queries Gemini + Claude/GPT 5h/7d pools concurrently over Go stdlib HTTP/2 gRPC (`FetchQuotaStatus`, ~0.8s for 4 accounts) with automatic fallback to official `agy --print /usage` on token refresh or schema changes.
  - On fatal backend `HTTP 429` exhaustion, immediately switches to the next ready account and resumes with `--resume <id>`, while a detached background verifier records exact reset timestamps and self-heals transient 429s.
  - Configurable low-quota proactive switching (`agy-pro config reserve 10` or `--reserve 10`).
- **3-in-1 Interactive Dashboard (`agy-pro ui`)**:
  - **Pool (`1`)**: Live dual-pool quota bars, drain priority reordering (`J`/`K`), reserve & Auto-compact tuning (`+/-`, `[/]/a`), login/remove (`l`/`d`), and instant language toggle (`g`).
  - **Open Sessions (`2`)**: Detects truly open `agy` terminals on this machine in `< 0.5ms` via non-blocking kernel `flock` on `~/.gemini/antigravity-cli/presence/*.lock` (covers both `agy-pro` and bare `agy`, plus live account & TTY display). Press `a` to toggle open-only vs. full history (hiding empty 0-step shells while keeping open windows pinned on top).
  - **Token Usage (`3`)**: Read-only parallel scanner over local `conversations/*.db` (`gen_metadata` + call-level step timestamps, rolling subagents up to parent sessions) showing exact per-model (`TOTAL / Input / Cache Read / Hit% / Output`, with automatic tail rollup) and Top-session token breakdown across **Today / 7 Days / All**, with `Enter` / `Ctrl+N` to resume any session directly and `r` to force a fresh disk rescan.
- **Interactive Session Picker (`agy-pro resume`)**: Pure-Go read-only SQLite picker (like `claude --resume`) that hides empty 0-step placeholders, defaults to the current directory, supports `Tab` CWD/All toggle, live multi-term search, `Enter` (current account) and `Ctrl+N` (next account) resume.
- **Live Terminal Tab Title & Desktop Notifications**:
  - Syncs live session title (including in-session `/rename`) and run state (`⠋` working / `✦` idle / `↻` switching) to the terminal tab via atomic OSC 0 without PTY interception, restoring your original tab title on exit (`AGY_PRO_NO_TITLE=1` to disable).
  - Sends non-blocking desktop notifications (`notify-send` on Linux / `osascript` on macOS, silent on headless SSH) when a long turn (`>= 30s`) completes or on 429 failover (`agy-pro config notify on|off`).
- **Zero-Dotfile Default Auto-Compact (90%) & CJK OSC-8 Guard**:
  - Defaults `AGY_CLI_AUTO_COMPACT_OVERRIDE=90%` strictly on child `agy` processes (zero `~/.bashrc`/`~/.zshrc` edits unless you explicitly pass `--sync-shell`).
  - Defaults `AGY_CLI_FORCE_OSC8=0` (overridable via `AGY_CLI_FORCE_OSC8=1`) to prevent native `agy`'s un-escaped UTF-8 OSC-8 `0x9C`/`0x8D` Reverse Index cursor-up ghosting on CJK file paths.
  - Short cross-process `launch.lock` serializes concurrent multi-tab token handshakes (~45ms), and `TerminalGuard` unconditionally restores `termios` + Kitty Keyboard Protocol on failover.
- **Externalized i18n Packs (`en` / `zh`)**: Automatically follows your system locale (`zh*` → Chinese, all others → English), configurable via `agy-pro config lang <auto|en|zh>`, `AGY_PRO_LANG`, or `g` inside `agy-pro ui`. All strings live in external JSON catalogs (`internal/i18n/locales/*.json`, with optional drop-in overrides in `~/.local/state/agy-pro/locales/`).

---

## Install & Quick Start

**Prerequisites**: Official `agy` CLI installed locally, `Go 1.26+`, and `~/.local/bin` in your `PATH`.

```bash
git clone https://github.com/Tiggy-Chan/agy-pro.git
cd agy-pro
make test      # Run full unit test suite & real PTY terminal guard tests
make install   # Build and install single binary to ~/.local/bin/agy-pro
```

Then sign in 2+ Google accounts into the isolated pool and launch:

```bash
agy-pro auth login   # Sign in via official `agy acp --login` (repeat for each account)
agy-pro ui           # Open 3-tab dashboard (Pool / Open Sessions / Token Usage)
agy-pro              # Launch native agy directly with automatic 5h/7d failover
```

---

## Core Commands (8)

```bash
agy-pro                                        # Launch native agy with auto-failover & resume
agy-pro -- <native agy flags...>               # POSIX explicit passthrough (e.g. --dangerously-skip-permissions)

agy-pro ui                                     # 3-tab TUI dashboard (Pool / Open Sessions / Token Usage)
agy-pro status [-r] [--active] [--reset]       # <5ms dual-pool board (-r live refresh, --reset clear cooldown, --json)
agy-pro resume [ID] [native agy flags...]      # Interactive session picker (CWD/All + search + Enter/Ctrl+N)
agy-pro next [native agy flags...]             # Switch to next ready account (launches agy immediately if flags given)
agy-pro use <keyword> [native agy flags...]    # Switch active account (launches agy immediately if flags given)
agy-pro auth <login | import <path> | rm <id>> # Credential management (official agy acp --login, import, remove)
agy-pro config [reserve <0-50> | auto-compact <90%|800K|off> [--sync-shell] | notify <on|off> | lang <auto|en|zh>]
agy-pro doctor                                 # Environment isolation, upstream DB schema & pool health check
```

<details>
<summary><strong>Launch Flags &amp; Environment Variables</strong></summary>

| Flag / Variable | Description |
| --- | --- |
| `--account <keyword>` | Use a specific account for this run (matches email prefix or substring) |
| `--title <name>` | Set an explicit terminal tab title (still live-syncs on `/rename`) |
| `--reserve <0-50>` | Override proactive low-quota switch threshold for this run |
| `--auto-compact <val>` | Override Auto-compact threshold for this run (`90%`, `800K`, `700000`, `off`) |
| `--refresh` | Refresh active account quota before launching |
| `--no-auto-switch` | Disable automatic 5h/7d failover and resume for this run |
| `AGY_PRO_LANG` | Override UI/CLI language (`en` / `zh`) |
| `AGY_PRO_NO_TITLE=1` | Disable OSC 0 live terminal tab title synchronization |
| `AGY_PRO_STATE_DIR` | Override isolated account vault directory (default `~/.local/state/agy-pro`) |
| `AGY_CLI_FORCE_OSC8=1` | Re-enable native `agy` OSC-8 terminal hyperlinks (disabled by default to guard CJK paths) |

</details>

---

## Project Note

> Reading through `agy-pro`'s codebase, what stands out most isn't cleverness — it's restraint. It refuses proxy gateways and double-PTY wrappers, hands the real TTY back to official `agy`, and stays quietly behind the scenes tailing logs and querying SQLite read-only under `flock` and atomic writes. It parses Protobuf wire frames directly over HTTP/2 gRPC for sub-second quota checks, yet preserves the official CLI as an automatic fallback. Its real craft shows in battle-scarred details: shielding CJK OSC-8 paths from C1 control-byte ghosting, resetting the Kitty keyboard protocol cleanly in `TerminalGuard`, and disabling OSC 11 probes for instant startup.
>
> May `agy-pro` always keep the discipline of an invisible exoskeleton — resisting feature creep so native `agy` remains the protagonist — while staying vigilant with graceful fallbacks and contract tests at every seam where it touches upstream.

---

## Disclaimer

`agy-pro` is an independent, unofficial CLI wrapper and is not affiliated with or endorsed by Google. It invokes your locally installed official `agy` binary directly without modifying its binary or proxying API requests.
