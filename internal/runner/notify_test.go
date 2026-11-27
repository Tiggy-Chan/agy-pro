package runner

import (
	"strings"
	"testing"
	"time"
)

func TestNotifierThresholdAndFailover(t *testing.T) {
	t.Setenv("AGY_PRO_NO_NOTIFY", "")
	var sent [][2]string
	n := NewNotifier(true, 30*time.Second)
	n.sendFn = func(title, body string) {
		sent = append(sent, [2]string{title, body})
	}

	t0 := time.Date(2026, 11, 27, 15, 0, 0, 0, time.UTC)
	// Short 10s turn must NOT spam desktop notification
	n.Observe("快速问答", true, t0)
	n.Observe("快速问答", false, t0.Add(10*time.Second))
	if len(sent) != 0 {
		t.Fatalf("expected short turn (<30s) not to notify, got %v", sent)
	}

	// Long 45s turn MUST fire one clean completion notification
	n.Observe("长任务构建", true, t0.Add(20*time.Second))
	n.Observe("长任务构建", true, t0.Add(50*time.Second))
	n.Observe("长任务构建", false, t0.Add(65*time.Second))
	if len(sent) != 1 || !strings.Contains(sent[0][0], "任务已完成") || !strings.Contains(sent[0][1], "长任务构建") || !strings.Contains(sent[0][1], "45s") {
		t.Fatalf("expected completion notification for 45s turn, got %v", sent)
	}

	// Repeated idle observations must not re-fire
	n.Observe("长任务构建", false, t0.Add(70*time.Second))
	if len(sent) != 1 {
		t.Fatalf("expected no duplicate notification while staying idle, got %v", sent)
	}

	// 429 Failover notification fires cleanly
	n.NotifyFailover("a@example.com", "b@example.com", "重构任务")
	if len(sent) != 2 || !strings.Contains(sent[1][0], "自动切号") || !strings.Contains(sent[1][1], "a@example.com → b@example.com") {
		t.Fatalf("unexpected failover notification: %v", sent)
	}

	// Disabled via AGY_PRO_NO_NOTIFY=1 suppresses all notifications
	t.Setenv("AGY_PRO_NO_NOTIFY", "1")
	n2 := NewNotifier(true, 30*time.Second)
	n2.sendFn = func(title, body string) { sent = append(sent, [2]string{title, body}) }
	n2.Observe("x", true, t0)
	n2.Observe("x", false, t0.Add(2*time.Minute))
	n2.NotifyFailover("a", "b", "c")
	if len(sent) != 2 {
		t.Fatalf("expected AGY_PRO_NO_NOTIFY=1 to suppress notifications, got %v", sent)
	}
}
