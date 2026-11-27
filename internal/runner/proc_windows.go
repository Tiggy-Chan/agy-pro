//go:build windows

package runner

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func stopChildGraceful(proc *os.Process, waitCh <-chan error, _ time.Duration) error {
	if proc == nil {
		return nil
	}
	select {
	case err := <-waitCh:
		return err
	default:
	}
	// Windows does not support SIGTERM via os.Process.Signal; terminate directly without stalling failover.
	_ = proc.Kill()
	return <-waitCh
}

func notifyTerminationSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGTERM)
}
