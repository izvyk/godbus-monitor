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

type scriptRunner struct {
	mu     sync.Mutex
	states map[string]*triggerState
}

type triggerState struct {
	running bool
	pending bool
	lastRun time.Time
}

const maxOutputBytes = 64 * 1024

func newScriptRunner() *scriptRunner { return &scriptRunner{states: make(map[string]*triggerState)} }

func safeEnv() []string {
	keep := []string{"PATH", "HOME", "USER", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "WAYLAND_DISPLAY", "DISPLAY"}
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

func (r *scriptRunner) run(ctx context.Context, t Trigger, log *slog.Logger) {
	r.mu.Lock()
	s := r.states[t.Name]
	if s == nil {
		s = &triggerState{}
		r.states[t.Name] = s
	}
	if s.running {
		s.pending = true
		r.mu.Unlock()
		log.Debug("queued trigger event while command is running", "trigger", t.Name)
		return
	}
	if !s.lastRun.IsZero() && time.Since(s.lastRun) < time.Duration(t.DebounceMs)*time.Millisecond {
		s.pending = true
		r.mu.Unlock()
		log.Debug("queued trigger event inside debounce window", "trigger", t.Name)
		return
	}
	s.running = true
	s.pending = false
	s.lastRun = time.Now()
	r.mu.Unlock()

	go r.execute(ctx, t, log)
}

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
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	if ctx.Err() != nil {
		_ = terminateProcessGroup(cmd.Process)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			log.Error("trigger timed out; process group terminated", "trigger", t.Name, "timeout_sec", t.TimeoutSec)
		} else {
			log.Debug("trigger cancelled", "trigger", t.Name)
		}
		return
	}
	if err != nil {
		log.Error("trigger failed", "trigger", t.Name, "error", err, "output", out.String())
		return
	}
	log.Info("trigger completed", "trigger", t.Name)
	if len(out.b) > 0 {
		log.Debug("trigger output", "trigger", t.Name, "output", out.String())
	}
}

func terminateProcessGroup(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill process group: %w", err)
	}
	return nil
}

var _ io.Writer = (*cappedBuffer)(nil)
