//go:build !windows

package runner

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func stopChildGraceful(proc *os.Process, waitCh <-chan error, grace time.Duration) error {
	if proc == nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGTERM)
	select {
	case err := <-waitCh:
		return err
	case <-time.After(grace):
		_ = proc.Kill()
		return <-waitCh
	}
}

func notifyTerminationSignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGHUP)
}
