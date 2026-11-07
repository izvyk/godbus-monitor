package main

import (
	"context"
	"errors"
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
	dryRun bool
}

const (
	maxOutputBytes = 64 * 1024
	// killGrace is how long a cancelled process group gets to exit on SIGTERM,
	// and how long a finished command's background children may hold its output.
	killGrace = 2 * time.Second
)

func newScriptRunner() *scriptRunner { return &scriptRunner{states: make(map[string]*triggerState)} }

func safeEnv() []string {
	keep := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "WAYLAND_DISPLAY"}
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
	n := min(len(p), maxOutputBytes-len(w.b))
	w.b = append(w.b, p[:n]...)
	w.truncated = w.truncated || n < len(p)
	return len(p), nil
}

func (w *cappedBuffer) String() string {
	if w.truncated {
		return string(w.b) + "\n[output truncated]"
	}
	return string(w.b)
}

// startLocked transitions to a running state and launches the command. Caller
// must hold r.mu and have verified the trigger is neither running nor inside
// the debounce window.
func (r *scriptRunner) startLocked(ctx context.Context, s *triggerState, t Trigger, log *slog.Logger) {
	s.running = true
	s.lastRun = time.Now()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.execute(ctx, t, log)
	}()
}

func (r *scriptRunner) run(ctx context.Context, t Trigger, log *slog.Logger) {
	if ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[t.Name]
	if s == nil {
		s = &triggerState{}
		r.states[t.Name] = s
	}
	switch {
	case s.running:
		// A run is in flight; execute()'s defer will pay this debt.
		s.pending = true
		log.Debug("coalesced trigger event", "trigger", t.Name)
	case !s.lastRun.IsZero() && time.Since(s.lastRun) < time.Duration(t.DebounceMs)*time.Millisecond:
		// Inside the debounce window: schedule a single delayed run. Further
		// events in the window are absorbed by the already-pending timer.
		if !s.timerSet {
			s.timerSet = true
			r.wg.Add(1)
			go r.delayedRun(ctx, t, log)
		}
		log.Debug("debounced trigger event", "trigger", t.Name)
	default:
		r.startLocked(ctx, s, t, log)
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
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[t.Name]
	s.timerSet = false
	if s.running {
		// A run started while we were waiting; hand the debt to its defer.
		s.pending = true
		return
	}
	r.startLocked(ctx, s, t, log)
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
		if pending {
			r.run(parent, t, log)
		}
	}()

	// Checked here rather than in run so dry runs show real debounce behaviour.
	if r.dryRun {
		log.Info("dry run: would run", "trigger", t.Name, "argv", t.Argv)
		return
	}

	ctx, cancel := context.WithTimeout(parent, time.Duration(t.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.Argv[0], t.Argv[1:]...)
	cmd.Env = safeEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// SIGTERM the whole group so scripts can clean up, instead of CommandContext's
	// default of SIGKILLing only the direct child.
	cmd.Cancel = func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return nil
	}
	cmd.WaitDelay = killGrace
	var out cappedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		log.Error("trigger failed to start", "trigger", t.Name, "error", err)
		return
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		// WaitDelay only escalates to SIGKILL on the direct child.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	switch {
	case err == nil:
		log.Info("trigger completed", "trigger", t.Name)
		log.Debug("trigger output", "trigger", t.Name, "output", out.String())
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		log.Error("trigger timed out", "trigger", t.Name, "timeout_sec", t.TimeoutSec, "output", out.String())
	case ctx.Err() != nil:
		log.Debug("trigger cancelled", "trigger", t.Name)
	case errors.Is(err, exec.ErrWaitDelay):
		log.Warn("trigger completed but a background process kept its output open; redirect that process's output", "trigger", t.Name)
	default:
		log.Error("trigger failed", "trigger", t.Name, "error", err, "output", out.String())
	}
}

func (r *scriptRunner) wait() { r.wg.Wait() }
