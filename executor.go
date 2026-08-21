package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// triggerState tracks one trigger's execution/debounce state.
//
// Semantics: at most one process runs at a time per trigger, and events
// coalesce so a burst of D-Bus signals yields at most one follow-up run.
//
//	running  - a process is executing right now
//	pending  - a follow-up run is owed; execute()'s defer will pay it
//	timerSet - a delayedRun goroutine is counting down to pay the debt
//	lastRun  - when the last run actually started
type triggerState struct {
	running  bool
	pending  bool
	timerSet bool
	lastRun  time.Time
}

type scriptRunner struct {
	mu     sync.Mutex
	states map[string]*triggerState
	wg     sync.WaitGroup
}

const maxOutputBytes = 64 * 1024

func newScriptRunner() *scriptRunner { return &scriptRunner{states: make(map[string]*triggerState)} }

func safeEnv() []string {
	keep := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "WAYLAND_DISPLAY", "DISPLAY"}
	env := make([]string, 0, len(keep))
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

type cappedBuffer struct {
	b         []byte
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	remaining := maxOutputBytes - len(w.b)
	if remaining > 0 {
		if len(p) > remaining {
			w.b = append(w.b, p[:remaining]...)
			w.truncated = true
		} else {
			w.b = append(w.b, p...)
		}
	} else {
		w.truncated = true
	}
	return len(p), nil
}

func (w *cappedBuffer) String() string {
	if w.truncated {
		return string(w.b) + "\n[output truncated]"
	}
	return string(w.b)
}

// startLocked transitions to a running state. Caller must hold r.mu and have
// verified the trigger is neither running nor inside the debounce window.
func (r *scriptRunner) startLocked(s *triggerState) {
	s.running = true
	s.lastRun = time.Now()
	r.wg.Add(1)
}

func (r *scriptRunner) run(ctx context.Context, t Trigger, log *slog.Logger) {
	r.mu.Lock()
	s := r.states[t.Name]
	if s == nil {
		s = &triggerState{}
		r.states[t.Name] = s
	}
	switch {
	case s.running:
		// A run is in flight; execute()'s defer will pay this debt.
		s.pending = true
		r.mu.Unlock()
		log.Debug("coalesced trigger event", "trigger", t.Name)
	case !s.lastRun.IsZero() && time.Since(s.lastRun) < time.Duration(t.DebounceMs)*time.Millisecond:
		// Inside the debounce window: schedule a single delayed run. Further
		// events in the window are absorbed by the already-pending timer.
		if !s.timerSet {
			s.timerSet = true
			r.wg.Add(1)
			go r.delayedRun(ctx, t, log)
		}
		r.mu.Unlock()
		log.Debug("debounced trigger event", "trigger", t.Name)
	default:
		r.startLocked(s)
		r.mu.Unlock()
		r.executeAsync(ctx, t, log)
	}
}

// delayedRun waits out the debounce window, then pays the debt by starting a
// run. Must be called with the waitgroup already incremented and s.timerSet
// set. Exactly one delayedRun per trigger is ever outstanding.
func (r *scriptRunner) delayedRun(ctx context.Context, t Trigger, log *slog.Logger) {
	defer r.wg.Done()
	timer := time.NewTimer(time.Duration(t.DebounceMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		r.mu.Lock()
		r.states[t.Name].timerSet = false
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	s := r.states[t.Name]
	s.timerSet = false
	if s.running {
		// A run started while we were waiting; hand the debt to its defer.
		s.pending = true
		r.mu.Unlock()
		return
	}
	r.startLocked(s)
	r.mu.Unlock()
	r.executeAsync(ctx, t, log)
}

func (r *scriptRunner) executeAsync(ctx context.Context, t Trigger, log *slog.Logger) {
	go func() {
		defer r.wg.Done()
		r.execute(ctx, t, log)
	}()
}

// execute runs the trigger command. The caller must have already set
// s.running = true via startLocked.
func (r *scriptRunner) execute(parent context.Context, t Trigger, log *slog.Logger) {
	defer func() {
		r.mu.Lock()
		s := r.states[t.Name]
		s.running = false
		pending := s.pending
		s.pending = false
		r.mu.Unlock()
		if pending && parent.Err() == nil {
			r.run(parent, t, log)
		}
	}()

	ctx, cancel := context.WithTimeout(parent, time.Duration(t.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Argv[0], t.Argv[1:]...)
	cmd.Env = safeEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		log.Error("trigger failed to start", "trigger", t.Name, "error", err)
		return
	}
	// CommandContext kills only the direct child on timeout, not the whole
	// group we created with Setpgid. We handle group termination ourselves in
	// the ctx.Done() branch; WaitDelay keeps cmd.Wait() from hanging if the
	// child double-forks.
	cmd.WaitDelay = 500 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			log.Error("trigger failed", "trigger", t.Name, "error", err, "output", out.String())
		} else if len(out.b) > 0 {
			log.Debug("trigger completed", "trigger", t.Name, "output", out.String())
		} else {
			log.Info("trigger completed", "trigger", t.Name)
		}
	case <-ctx.Done():
		if err := terminateProcessGroup(cmd.Process); err != nil {
			log.Warn("failed to terminate process group", "trigger", t.Name, "error", err)
		}
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Error("trigger timed out", "trigger", t.Name, "timeout_sec", t.TimeoutSec, "output", out.String())
		} else {
			log.Debug("trigger cancelled", "trigger", t.Name)
		}
	}
}

func (r *scriptRunner) wait() { r.wg.Wait() }

func terminateProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("send SIGTERM: %w", err)
	}
	// Give the group a moment to exit on SIGTERM before escalating, but don't
	// sleep unconditionally when the process is already gone.
	for range 20 {
		if err := syscall.Kill(-p.Pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("send SIGKILL: %w", err)
	}
	return nil
}

var _ io.Writer = (*cappedBuffer)(nil)
