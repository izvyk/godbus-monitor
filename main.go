package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

type wrappedSignal struct {
	Bus    string
	Signal *dbus.Signal
}

func unwrapVariant(v any) any {
	for {
		x, ok := v.(dbus.Variant)
		if !ok {
			return v
		}
		v = x.Value()
	}
}

func evaluateCondition(actual any, operator, expected string) bool {
	actual = unwrapVariant(actual)
	switch v := actual.(type) {
	case string:
		switch operator {
		case "==":
			return v == expected
		case "!=":
			return v != expected
		}
	case bool:
		e, err := strconv.ParseBool(expected)
		if err != nil {
			return false
		}
		switch operator {
		case "==":
			return v == e
		case "!=":
			return v != e
		}
	default:
		if f, ok := asFloat(actual); ok {
			return compareNumber(f, operator, expected)
		}
	}
	return false
}

// asFloat converts any numeric value to float64. Reflection keeps this to a
// few lines instead of one switch arm per integer width, and covers kinds the
// hand-written list missed (notably uint8, which is D-Bus's "byte" type).
func asFloat(v any) (float64, bool) {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	}
	return 0, false
}

func compareNumber(actual float64, operator, expected string) bool {
	e, err := strconv.ParseFloat(expected, 64)
	if err != nil {
		return false
	}
	switch operator {
	case "==":
		return actual == e
	case "!=":
		return actual != e
	case ">":
		return actual > e
	case "<":
		return actual < e
	default:
		return false
	}
}

type connRegistry struct {
	mu    sync.RWMutex
	conns map[string]*dbus.Conn
}

func newConnRegistry() *connRegistry { return &connRegistry{conns: make(map[string]*dbus.Conn)} }
func (r *connRegistry) set(name string, c *dbus.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c == nil {
		delete(r.conns, name)
	} else {
		r.conns[name] = c
	}
}
func (r *connRegistry) get(name string) *dbus.Conn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[name]
}

func (r *connRegistry) fetchProperty(ctx context.Context, bus, sender, path, iface, prop string) (any, error) {
	conn := r.get(bus)
	if conn == nil {
		return nil, fmt.Errorf("no live connection on %s bus", bus)
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var v dbus.Variant
	call := conn.Object(sender, dbus.ObjectPath(path)).CallWithContext(callCtx, "org.freedesktop.DBus.Properties.Get", 0, iface, prop)
	if err := call.Store(&v); err != nil {
		return nil, err
	}
	return v.Value(), nil
}

// nameOwner resolves a well-known bus name (e.g. "org.freedesktop.login1")
// to its current unique owner (e.g. ":1.42").
func (r *connRegistry) nameOwner(ctx context.Context, bus, name string) (string, error) {
	conn := r.get(bus)
	if conn == nil {
		return "", fmt.Errorf("no live connection on %s bus", bus)
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var owner string
	err := conn.BusObject().CallWithContext(callCtx, "org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner)
	return owner, err
}

type busWatcher struct {
	name     string
	connect  func() (*dbus.Conn, error)
	triggers []Trigger
	events   chan<- wrappedSignal
	registry *connRegistry
	log      *slog.Logger
}

func (w *busWatcher) run(ctx context.Context) {
	backoff := time.Second
	defer w.registry.set(w.name, nil)
	for ctx.Err() == nil {
		conn, err := w.connect()
		if err != nil {
			w.log.Error("bus connect failed", "bus", w.name, "error", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		w.registry.set(w.name, conn)
		retry := w.watch(ctx, conn)
		w.registry.set(w.name, nil)
		_ = conn.Close()
		if !retry {
			return
		}
		w.log.Warn("bus connection lost, reconnecting", "bus", w.name)
	}
}

func (w *busWatcher) watch(ctx context.Context, conn *dbus.Conn) bool {
	interfaces := make(map[string]bool)
	for _, t := range w.triggers {
		interfaces[t.Interface] = true
	}
	var registered []string
	for iface := range interfaces {
		if err := conn.AddMatchSignal(
			dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
			dbus.WithMatchMember("PropertiesChanged"),
			dbus.WithMatchArg(0, iface),
		); err != nil {
			w.log.Error("failed to register match rule", "bus", w.name, "interface", iface, "error", err)
			for _, prev := range registered {
				_ = conn.RemoveMatchSignal(
					dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
					dbus.WithMatchMember("PropertiesChanged"),
					dbus.WithMatchArg(0, prev),
				)
			}
			return true
		}
		registered = append(registered, iface)
	}

	ch := make(chan *dbus.Signal, 256)
	conn.Signal(ch)
	w.log.Info("watching bus", "bus", w.name, "triggers", len(w.triggers))
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				return true
			}
			select {
			case w.events <- wrappedSignal{Bus: w.name, Signal: sig}:
			case <-ctx.Done():
				conn.RemoveSignal(ch)
				return false
			}
		case <-ctx.Done():
			conn.RemoveSignal(ch)
			return false
		}
	}
}

// senderMatches reports whether a signal from actual matches the trigger's
// sender filter. Unique names (":1.42") match only themselves. Well-known
// names ("org.freedesktop.login1") are resolved to the current owner via
// GetNameOwner, so configs stay portable across machines. Resolved owners are
// memoized per signal to avoid repeated bus round-trips.
func senderMatches(ctx context.Context, registry *connRegistry, bus, filter, actual string, memo map[string]string, log *slog.Logger) bool {
	if filter == actual {
		return true
	}
	if strings.HasPrefix(filter, ":") {
		return false
	}
	owner, ok := memo[filter]
	if !ok {
		var err error
		owner, err = registry.nameOwner(ctx, bus, filter)
		if err != nil {
			log.Debug("failed to resolve sender name", "name", filter, "bus", bus, "error", err)
			owner = ""
		}
		memo[filter] = owner
	}
	return owner != "" && owner == actual
}

type valueTracker struct {
	values map[string]any
}

func newValueTracker() *valueTracker {
	return &valueTracker{values: make(map[string]any)}
}

// hasChanged records the latest value for a trigger and reports whether it
// differs from the previous observation. The first observation counts as a
// change so a daemon started before the first relevant signal does not miss it.
func (t *valueTracker) hasChanged(triggerName string, actual any) bool {
	actual = unwrapVariant(actual)
	previous, seen := t.values[triggerName]
	t.values[triggerName] = actual
	return !seen || !reflect.DeepEqual(previous, actual)
}

func handleSignal(ctx context.Context, e wrappedSignal, triggers []Trigger, runner *scriptRunner, registry *connRegistry, tracker *valueTracker, log *slog.Logger) {
	if e.Signal == nil || len(e.Signal.Body) < 2 {
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
	senderMemo := make(map[string]string)
	for _, t := range triggers {
		if t.Bus != e.Bus || t.Interface != iface || (t.Path != "" && string(e.Signal.Path) != t.Path) {
			continue
		}
		if t.Sender != "" && !senderMatches(ctx, registry, e.Bus, t.Sender, e.Signal.Sender, senderMemo, log) {
			continue
		}
		if v, exists := changed[t.Property]; exists {
			checkAndRun(ctx, t, v.Value(), runner, tracker, log)
			continue
		}
		if !slices.Contains(invalidated, t.Property) {
			continue
		}
		v, err := registry.fetchProperty(ctx, e.Bus, e.Signal.Sender, string(e.Signal.Path), t.Interface, t.Property)
		if err != nil {
			log.Warn("failed to fetch invalidated property", "trigger", t.Name, "error", err)
			continue
		}
		checkAndRun(ctx, t, v, runner, tracker, log)
	}
}

func checkAndRun(ctx context.Context, t Trigger, actual any, runner *scriptRunner, tracker *valueTracker, log *slog.Logger) {
	if t.OnlyOnChange && !tracker.hasChanged(t.Name, actual) {
		log.Debug("ignored unchanged property value", "trigger", t.Name, "value", fmt.Sprintf("%v", actual))
		return
	}
	if evaluateCondition(actual, t.Operator, t.ExpectedValue) {
		log.Info("trigger matched", "trigger", t.Name, "value", fmt.Sprintf("%v", actual))
		runner.run(ctx, t, log)
	}
}

// startDispatcher drains the shared events channel and routes each signal to
// handleSignal. Without this the watchers block on a full channel and no
// trigger ever fires.
func startDispatcher(ctx context.Context, events <-chan wrappedSignal, triggers []Trigger, runner *scriptRunner, registry *connRegistry, log *slog.Logger) {
	tracker := newValueTracker()
	go func() {
		for {
			select {
			case e, ok := <-events:
				if !ok {
					return
				}
				handleSignal(ctx, e, triggers, runner, registry, tracker, log)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func main() {
	configPath := flag.String("config", "triggers.json", "path to JSON trigger config")
	validateOnly := flag.Bool("validate", false, "validate config and exit")
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
	if *validateOnly {
		fmt.Printf("config OK: %d trigger(s)\n", len(triggers))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	events := make(chan wrappedSignal, 256)
	registry := newConnRegistry()
	runner := newScriptRunner()
	startDispatcher(ctx, events, triggers, runner, registry, logger)
	var wg sync.WaitGroup
	byBus := map[string][]Trigger{"system": {}, "session": {}}
	for _, t := range triggers {
		byBus[t.Bus] = append(byBus[t.Bus], t)
	}

	startBus := func(name string, connect func() (*dbus.Conn, error)) {
		if len(byBus[name]) == 0 {
			return
		}
		ts := append([]Trigger(nil), byBus[name]...)
		slices.SortFunc(ts, func(a, b Trigger) int { return strings.Compare(a.Name, b.Name) })
		w := &busWatcher{name: name, connect: connect, triggers: ts, events: events, registry: registry, log: logger}
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}

	startBus("system", dbus.SystemBus)
	startBus("session", dbus.SessionBus)
	logger.Info("generic D-Bus trigger daemon running")
	<-ctx.Done()
	wg.Wait()
	runner.wait()
	logger.Info("stopped")
}
