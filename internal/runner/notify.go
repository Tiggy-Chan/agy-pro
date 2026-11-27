package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Tiggy-Chan/agy-pro/internal/i18n"
)

const DefaultNotifyMinDuration = 30 * time.Second

// Notifier dispatches lightweight, non-blocking desktop notifications when long-running
// agent turns finish (RUNNING -> IDLE after >= MinDuration) or when 429 failover triggers.
// Completely silent on headless SSH terminals or when disabled via config / AGY_PRO_NO_NOTIFY=1.
type Notifier struct {
	enabled      bool
	minDuration  time.Duration
	wasRunning   bool
	runningSince time.Time
	sendFn       func(title, body string)
}

func NewNotifier(enabled bool, minDuration time.Duration) *Notifier {
	if os.Getenv("AGY_PRO_NO_NOTIFY") == "1" {
		enabled = false
	}
	if minDuration <= 0 {
		minDuration = DefaultNotifyMinDuration
	}
	return &Notifier{
		enabled:     enabled,
		minDuration: minDuration,
		sendFn:      dispatchDesktopNotify,
	}
}

func (n *Notifier) Observe(title string, running bool, now time.Time) {
	if n == nil || !n.enabled {
		return
	}
	if running {
		if !n.wasRunning {
			n.wasRunning = true
			n.runningSince = now
		}
		return
	}
	if n.wasRunning {
		elapsed := now.Sub(n.runningSince)
		n.wasRunning = false
		n.runningSince = time.Time{}
		if elapsed >= n.minDuration {
			label := sanitizeTabTitle(title)
			if label == "" {
				label = i18n.T("notify.default_session")
			}
			n.send(i18n.T("notify.done_title"), i18n.T("notify.done_body", label, fmtNotifyDuration(elapsed)))
		}
	}
}

func (n *Notifier) NotifyFailover(oldEmail, newEmail, title string) {
	if n == nil || !n.enabled {
		return
	}
	label := sanitizeTabTitle(title)
	body := fmt.Sprintf("%s → %s", oldEmail, newEmail)
	if label != "" {
		body += " · " + label
	}
	n.send(i18n.T("notify.failover_title"), body)
}

func (n *Notifier) send(title, body string) {
	if n == nil || !n.enabled || n.sendFn == nil {
		return
	}
	n.sendFn(title, body)
}

func fmtNotifyDuration(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	m := sec / 60
	s := sec % 60
	if m < 60 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%dh%dm", m/60, m%60)
}

func dispatchDesktopNotify(title, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			if _, err := exec.LookPath("osascript"); err != nil {
				return
			}
			// Pass title & body via argv to avoid any AppleScript string injection.
			script := `on run argv
display notification (item 2 of argv) with title (item 1 of argv)
end run`
			cmd = exec.CommandContext(ctx, "osascript", "-e", script, title, body)
		case "linux":
			if strings.TrimSpace(os.Getenv("DISPLAY")) == "" && strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) == "" {
				return
			}
			bin, err := exec.LookPath("notify-send")
			if err != nil {
				return
			}
			cmd = exec.CommandContext(ctx, bin, "-a", "agy-pro", "-u", "normal", "--", title, body)
		default:
			return
		}
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		_ = cmd.Run()
	}()
}
