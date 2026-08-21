// godbus-monitor watches org.freedesktop.DBus.Properties.PropertiesChanged
// signals on the system and/or session bus and runs a shell command when a
// watched property matches a configured value. Rules come from a JSON file
// (see triggers.json for the shape).
//
// Design notes (why it's structured this way):
//   - Each bus (system/session) gets its own watcher goroutine that
//     reconnects with backoff if the connection drops, instead of dying.
//   - Match rules are deduplicated per (bus, interface) so we don't register
//     the same rule N times for N triggers on the same interface.
//   - PropertiesChanged's "invalidated" list is handled: some services
//     (systemd-logind among them) report a property as invalidated rather
//     than inlining its new value, which the naive version silently missed.
//   - Script execution is debounced and serialized per-trigger, with a
//     timeout, and runs with a minimal explicit environment rather than the
//     daemon's full one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

type wrappedSignal struct {
	Bus    string
	Signal *dbus.Signal
}

func evaluateCondition(actual interface{}, operator, expectedStr string) bool {
	actualStr := fmt.Sprintf("%v", actual)

	switch operator {
	case "==", "":
		return actualStr == expectedStr
	case "!=":
		return actualStr != expectedStr
	}

	actualFloat, err1 := strconv.ParseFloat(actualStr, 64)
	expectedFloat, err2 := strconv.ParseFloat(expectedStr, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	switch operator {
	case ">":
		return actualFloat > expectedFloat
	case "<":
		return actualFloat < expectedFloat
	}
	return false
}

// connRegistry holds the currently-live *dbus.Conn per bus name, so the
// event-handling goroutine can issue a synchronous Properties.Get when a
// signal reports a property as invalidated rather than inlining its value.
type connRegistry struct {
	mu    sync.RWMutex
	conns map[string]*dbus.Conn
}

func newConnRegistry() *connRegistry { return &connRegistry{conns: make(map[string]*dbus.Conn)} }

func (r *connRegistry) set(name string, c *dbus.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[name] = c
}

func (r *connRegistry) get(name string) *dbus.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[name]
}

func (r *connRegistry) fetchProperty(bus, sender, path, iface, prop string) (interface{}, error) {
	conn := r.get(bus)
	if conn == nil {
		return nil, fmt.Errorf("no live connection on %s bus", bus)
	}
	obj := conn.Object(sender, dbus.ObjectPath(path))
	var variant dbus.Variant
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, iface, prop).Store(&variant); err != nil {
		return nil, err
	}
	return variant.Value(), nil
}

// busWatcher owns one bus connection, reconnecting with backoff if it drops
// and re-registering match rules each time it (re)connects.
type busWatcher struct {
	name       string // "system" or "session"
	connect    func() (*dbus.Conn, error)
	interfaces []string
	events     chan<- wrappedSignal
	registry   *connRegistry
	log        *slog.Logger
}

func (w *busWatcher) run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for ctx.Err() == nil {
		conn, err := w.connect()
		if err != nil {
			w.log.Error("bus connect failed", "bus", w.name, "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		backoff = time.Second // reset once we successfully connect
		w.registry.set(w.name, conn)
		droppedNotCancelled := w.watch(ctx, conn)
		w.registry.set(w.name, nil)
		conn.Close()

		if !droppedNotCancelled {
			return
		}
		w.log.Warn("bus connection lost, reconnecting", "bus", w.name)
	}
}

// watch subscribes on a live connection and forwards signals until the
// connection drops or ctx is cancelled. Returns true if the bus dropped
// (caller should retry), false if ctx was cancelled (caller should stop).
func (w *busWatcher) watch(ctx context.Context, conn *dbus.Conn) bool {
	for _, iface := range w.interfaces {
		if err := conn.AddMatchSignal(
			dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
			dbus.WithMatchMember("PropertiesChanged"),
			dbus.WithMatchArg(0, iface),
		); err != nil {
			w.log.Error("failed to register match rule", "bus", w.name, "interface", iface, "error", err)
		}
	}

	ch := make(chan *dbus.Signal, 20)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)

	w.log.Info("watching bus", "bus", w.name, "interfaces", w.interfaces)

	for {
		select {
		case sig, open := <-ch:
			if !open {
				return true
			}
			select {
			case w.events <- wrappedSignal{Bus: w.name, Signal: sig}:
			case <-ctx.Done():
				return false
			}
		case <-ctx.Done():
			return false
		}
	}
}

func handleSignal(ctx context.Context, e wrappedSignal, triggers []Trigger, runner *scriptRunner, registry *connRegistry, log *slog.Logger) {
	if len(e.Signal.Body) < 2 {
		return
	}
	iface, ok := e.Signal.Body[0].(string)
	if !ok {
		return
	}
	changed, ok := e.Signal.Body[1].(map[string]dbus.Variant)
	if !ok {
		return
	}
	var invalidated []string
	if len(e.Signal.Body) >= 3 {
		invalidated, _ = e.Signal.Body[2].([]string)
	}

	for _, t := range triggers {
		if t.Bus != e.Bus || t.Interface != iface {
			continue
		}

		if variant, exists := changed[t.Property]; exists {
			checkAndRun(ctx, t, variant.Value(), runner, log)
			continue
		}

		for _, name := range invalidated {
			if name != t.Property {
				continue
			}
			val, err := registry.fetchProperty(e.Bus, e.Signal.Sender, string(e.Signal.Path), t.Interface, t.Property)
			if err != nil {
				log.Warn("failed to fetch invalidated property", "trigger", t.Name, "error", err)
				continue
			}
			checkAndRun(ctx, t, val, runner, log)
		}
	}
}

func checkAndRun(ctx context.Context, t Trigger, actual interface{}, runner *scriptRunner, log *slog.Logger) {
	if evaluateCondition(actual, t.Operator, t.ExpectedValue) {
		log.Info("trigger matched", "trigger", t.Name, "interface", t.Interface, "property", t.Property, "value", fmt.Sprintf("%v", actual))
		go runner.run(ctx, t, log)
	}
}

func main() {
	configPath := flag.String("config", "triggers.json", "path to JSON trigger config")
	validateOnly := flag.Bool("validate", false, "parse and validate the config, then exit")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	triggers, err := loadConfig(*configPath)
	if err != nil {
		logger.Error("config error", "error", err)
		os.Exit(1)
	}
	logger.Info("config loaded", "triggers", len(triggers))

	if *validateOnly {
		fmt.Printf("config OK: %d trigger(s)\n", len(triggers))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	events := make(chan wrappedSignal, 50)
	registry := newConnRegistry()
	runner := newScriptRunner()
	var wg sync.WaitGroup

	ifaceSet := map[string]map[string]bool{"system": {}, "session": {}}
	for _, t := range triggers {
		ifaceSet[t.Bus][t.Interface] = true
	}

	startBus := func(name string, connect func() (*dbus.Conn, error)) {
		if len(ifaceSet[name]) == 0 {
			return
		}
		ifaces := make([]string, 0, len(ifaceSet[name]))
		for i := range ifaceSet[name] {
			ifaces = append(ifaces, i)
		}
		w := &busWatcher{name: name, connect: connect, interfaces: ifaces, events: events, registry: registry, log: logger}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.run(ctx)
		}()
	}

	startBus("system", dbus.SystemBus)
	startBus("session", dbus.SessionBus)

	logger.Info("⚡ generic D-Bus trigger daemon running")

loop:
	for {
		select {
		case e := <-events:
			handleSignal(ctx, e, triggers, runner, registry, logger)
		case <-ctx.Done():
			logger.Info("shutting down")
			break loop
		}
	}

	wg.Wait()
	logger.Info("stopped")
}
