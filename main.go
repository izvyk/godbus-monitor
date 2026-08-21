package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
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

func evaluateCondition(actual interface{}, operator, expected string) bool {
	actualStr := fmt.Sprintf("%v", actual)
	switch operator {
	case "==":
		return actualStr == expected
	case "!=":
		return actualStr != expected
	}
	a, e := strconv.ParseFloat(actualStr, 64)
	b, err := strconv.ParseFloat(expected, 64)
	if e != nil || err != nil {
		return false
	}
	if operator == ">" {
		return a > b
	}
	if operator == "<" {
		return a < b
	}
	return false
}

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

func (r *connRegistry) fetchProperty(ctx context.Context, bus, sender, path, iface, prop string) (interface{}, error) {
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

type busWatcher struct {
	name       string
	connect    func() (*dbus.Conn, error)
	interfaces []string
	events     chan<- wrappedSignal
	registry   *connRegistry
	log        *slog.Logger
}

func (w *busWatcher) run(ctx context.Context) {
	backoff := time.Second
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
	for _, iface := range w.interfaces {
		if err := conn.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchArg(0, iface)); err != nil {
			w.log.Error("failed to register match rule; reconnecting", "bus", w.name, "interface", iface, "error", err)
			return true
		}
	}
	ch := make(chan *dbus.Signal, 256)
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
	for _, t := range triggers {
		if t.Bus != e.Bus || t.Interface != iface {
			continue
		}
		if v, exists := changed[t.Property]; exists {
			checkAndRun(ctx, t, v.Value(), runner, log)
			continue
		}
		for _, name := range invalidated {
			if name != t.Property {
				continue
			}
			v, err := registry.fetchProperty(ctx, e.Bus, e.Signal.Sender, string(e.Signal.Path), t.Interface, t.Property)
			if err != nil {
				log.Warn("failed to fetch invalidated property", "trigger", t.Name, "error", err)
				continue
			}
			checkAndRun(ctx, t, v, runner, log)
		}
	}
}
func checkAndRun(ctx context.Context, t Trigger, actual interface{}, runner *scriptRunner, log *slog.Logger) {
	if evaluateCondition(actual, t.Operator, t.ExpectedValue) {
		log.Info("trigger matched", "trigger", t.Name, "value", fmt.Sprintf("%v", actual))
		runner.run(ctx, t, log)
	}
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
		sort.Strings(ifaces)
		w := &busWatcher{name: name, connect: connect, interfaces: ifaces, events: events, registry: registry, log: logger}
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}
	startBus("system", dbus.SystemBus)
	startBus("session", dbus.SessionBus)
	logger.Info("generic D-Bus trigger daemon running")
	for {
		select {
		case e := <-events:
			handleSignal(ctx, e, triggers, runner, registry, logger)
		case <-ctx.Done():
			wg.Wait()
			logger.Info("stopped")
			return
		}
	}
}
