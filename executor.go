package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// scriptRunner serializes and debounces execution per-trigger. Without this,
// a burst of PropertiesChanged signals during a state transition (e.g. the
// lock/unlock storms during session transitions) fans out into a pile of
// overlapping shell processes fighting each other -- exactly the class of
// bug you hit building shell-event-hooks.
type scriptRunner struct {
	mu      sync.Mutex
	running map[string]bool
	lastRun map[string]time.Time
}

func newScriptRunner() *scriptRunner {
	return &scriptRunner{
		running: make(map[string]bool),
		lastRun: make(map[string]time.Time),
	}
}

// safeEnv is the environment handed to trigger scripts: an explicit
// allowlist rather than the daemon's full environment. Scripts here run
// unattended in response to external signals, so there's no reason to hand
// them anything beyond what's needed to talk to the session (D-Bus, the
// compositor) and find binaries.
func safeEnv() []string {
	keep := []string{
		"PATH", "HOME", "USER", "XDG_RUNTIME_DIR",
		"DBUS_SESSION_BUS_ADDRESS", "WAYLAND_DISPLAY", "DISPLAY",
	}
	env := make([]string, 0, len(keep))
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (r *scriptRunner) run(ctx context.Context, t Trigger, log *slog.Logger) {
	r.mu.Lock()
	if r.running[t.Name] {
		r.mu.Unlock()
		log.Warn("skipping trigger: previous run still in flight", "trigger", t.Name)
		return
	}
	if last, ok := r.lastRun[t.Name]; ok {
		if since := time.Since(last); since < time.Duration(t.DebounceMs)*time.Millisecond {
			r.mu.Unlock()
			log.Debug("skipping trigger: inside debounce window", "trigger", t.Name, "since_ms", since.Milliseconds())
			return
		}
	}
	r.running[t.Name] = true
	r.lastRun[t.Name] = time.Now()
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running[t.Name] = false
		r.mu.Unlock()
		if p := recover(); p != nil {
			log.Error("trigger handler panicked", "trigger", t.Name, "panic", p)
		}
	}()

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(t.TimeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", t.Script)
	cmd.Env = safeEnv()
	out, err := cmd.CombinedOutput()

	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		log.Error("trigger script timed out, killed", "trigger", t.Name, "timeout_sec", t.TimeoutSec)
	case err != nil:
		log.Error("trigger script failed", "trigger", t.Name, "error", err, "output", string(out))
	default:
		log.Info("trigger script ran", "trigger", t.Name)
		if len(out) > 0 {
			log.Debug("trigger script output", "trigger", t.Name, "output", string(out))
		}
	}
}
