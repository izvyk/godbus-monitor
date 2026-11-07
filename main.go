package main

import (
	"context"
	"errors"
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

const (
	propertiesInterface = "org.freedesktop.DBus.Properties"
	busCallTimeout      = 2 * time.Second
	minBackoff          = time.Second
	maxBackoff          = 30 * time.Second
)

// wrappedSignal is a signal plus the connection that delivered it, so handlers
// can call back over that same connection.
type wrappedSignal struct {
	Bus    string
	Conn   *dbus.Conn
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
	if path, ok := actual.(dbus.ObjectPath); ok {
		actual = string(path)
	}
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

// asFloat converts any numeric value to float64. Integers beyond 2^53 lose
// precision, which is acceptable for the thresholds triggers compare against.
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

func fetchProperty(ctx context.Context, conn *dbus.Conn, sig *dbus.Signal, iface, prop string) (any, error) {
	callCtx, cancel := context.WithTimeout(ctx, busCallTimeout)
	defer cancel()
	var v dbus.Variant
	call := conn.Object(sig.Sender, sig.Path).CallWithContext(callCtx, propertiesInterface+".Get", 0, iface, prop)
	if err := call.Store(&v); err != nil {
		return nil, err
	}
	return v.Value(), nil
}

// nameOwner resolves a well-known bus name (e.g. "org.freedesktop.login1")
// to its current unique owner (e.g. ":1.42").
func nameOwner(ctx context.Context, conn *dbus.Conn, name string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, busCallTimeout)
	defer cancel()
	var owner string
	err := conn.BusObject().CallWithContext(callCtx, "org.freedesktop.DBus.GetNameOwner", 0, name).Store(&owner)
	return owner, err
}

type busWatcher struct {
	name     string
	connect  func(...dbus.ConnOption) (*dbus.Conn, error)
	triggers []Trigger
	events   chan<- wrappedSignal
	log      *slog.Logger
}

// run keeps a watch alive until ctx is cancelled. Every retry backs off, not
// only failed connects: a bus that accepts connections but rejects a match rule
// would otherwise spin.
func (w *busWatcher) run(ctx context.Context) {
	backoff := minBackoff
	for {
		started := time.Now()
		conn, err := w.connect(dbus.WithContext(ctx))
		if err == nil {
			err = w.watch(ctx, conn)
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		// Only a connection that stayed up counts as having recovered.
		if time.Since(started) > maxBackoff {
			backoff = minBackoff
		}
		w.log.Warn("bus disconnected, retrying", "bus", w.name, "error", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func matchOptions(t Trigger) []dbus.MatchOption {
	if t.Signal != "" {
		return []dbus.MatchOption{dbus.WithMatchInterface(t.Interface), dbus.WithMatchMember(t.Signal)}
	}
	return []dbus.MatchOption{
		dbus.WithMatchInterface(propertiesInterface),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchArg(0, t.Interface),
	}
}

// watch forwards signals until the connection is lost or ctx is cancelled. The
// caller closes conn, which also drops its match rules and receiver. Triggers
// sharing a rule are harmless: the bus delivers a signal once per connection.
func (w *busWatcher) watch(ctx context.Context, conn *dbus.Conn) error {
	// Register the local receiver before match rules so newly subscribed
	// signals cannot arrive before there is a channel to receive them.
	ch := make(chan *dbus.Signal, 256)
	conn.Signal(ch)
	for _, t := range w.triggers {
		if err := conn.AddMatchSignal(matchOptions(t)...); err != nil {
			return fmt.Errorf("register match rule for trigger %q: %w", t.Name, err)
		}
	}
	w.log.Info("watching bus", "bus", w.name, "triggers", len(w.triggers))
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				return errors.New("connection lost")
			}
			select {
			case w.events <- wrappedSignal{Bus: w.name, Conn: conn, Signal: sig}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// senderMatches reports whether a signal from actual matches the trigger's
// sender filter. Unique names (":1.42") match only themselves. Well-known
// names ("org.freedesktop.login1") are resolved to the current owner via
// GetNameOwner, so configs stay portable across machines. Resolved owners are
// memoized per signal to avoid repeated bus round-trips.
func senderMatches(ctx context.Context, conn *dbus.Conn, filter, actual string, memo map[string]string, log *slog.Logger) bool {
	if filter == actual {
		return true
	}
	if strings.HasPrefix(filter, ":") {
		return false
	}
	owner, ok := memo[filter]
	if !ok {
		var err error
		owner, err = nameOwner(ctx, conn, filter)
		if err != nil {
			log.Debug("failed to resolve sender name", "name", filter, "error", err)
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

func handleSignal(ctx context.Context, e wrappedSignal, triggers []Trigger, runner *scriptRunner, tracker *valueTracker, log *slog.Logger) {
	if e.Signal == nil {
		return
	}
	var iface string
	var changed map[string]dbus.Variant
	var invalidated []string
	validProperties := false
	if e.Signal.Name == propertiesInterface+".PropertiesChanged" && len(e.Signal.Body) >= 2 {
		var validInterface, validChanged bool
		iface, validInterface = e.Signal.Body[0].(string)
		changed, validChanged = e.Signal.Body[1].(map[string]dbus.Variant)
		validProperties = validInterface && validChanged
		if len(e.Signal.Body) >= 3 {
			invalidated, _ = e.Signal.Body[2].([]string)
		}
	}
	senderMemo := make(map[string]string)
	for _, t := range triggers {
		if t.Bus != e.Bus || (t.Path != "" && string(e.Signal.Path) != t.Path) {
			continue
		}
		if t.Signal != "" {
			if e.Signal.Name != t.Interface+"."+t.Signal || t.Argument >= len(e.Signal.Body) {
				continue
			}
		} else if !validProperties || t.Interface != iface {
			continue
		}
		if t.Sender != "" && !senderMatches(ctx, e.Conn, t.Sender, e.Signal.Sender, senderMemo, log) {
			continue
		}
		if t.Signal != "" {
			checkAndRun(ctx, t, e.Signal.Body[t.Argument], runner, tracker, log)
			continue
		}
		if v, exists := changed[t.Property]; exists {
			checkAndRun(ctx, t, v.Value(), runner, tracker, log)
			continue
		}
		if !slices.Contains(invalidated, t.Property) {
			continue
		}
		v, err := fetchProperty(ctx, e.Conn, e.Signal, t.Interface, t.Property)
		if err != nil {
			log.Warn("failed to fetch invalidated property", "trigger", t.Name, "error", err)
			continue
		}
		checkAndRun(ctx, t, v, runner, tracker, log)
	}
}

func checkAndRun(ctx context.Context, t Trigger, actual any, runner *scriptRunner, tracker *valueTracker, log *slog.Logger) {
	if t.OnlyOnChange && !tracker.hasChanged(t.Name, actual) {
		log.Debug("ignored unchanged value", "trigger", t.Name, "value", actual)
		return
	}
	if !evaluateCondition(actual, t.Operator, t.ExpectedValue) {
		log.Debug("condition not met", "trigger", t.Name, "value", actual, "type", fmt.Sprintf("%T", actual))
		return
	}
	log.Info("trigger matched", "trigger", t.Name, "value", actual)
	runner.run(ctx, t, log)
}

// startDispatcher drains the shared events channel and routes each signal to
// handleSignal. The returned channel closes once it has stopped, after which
// no new runs can start.
func startDispatcher(ctx context.Context, events <-chan wrappedSignal, triggers []Trigger, runner *scriptRunner, log *slog.Logger) <-chan struct{} {
	tracker := newValueTracker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case e := <-events:
				handleSignal(ctx, e, triggers, runner, tracker, log)
			case <-ctx.Done():
				return
			}
		}
	}()
	return done
}

func main() {
	configPath := flag.String("config", "triggers.json", "path to JSON trigger config")
	validateOnly := flag.Bool("validate", false, "validate config and exit")
	dryRun := flag.Bool("dry-run", false, "watch the buses and log matches without running any command")
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
	runner := newScriptRunner()
	runner.dryRun = *dryRun
	dispatcherDone := startDispatcher(ctx, events, triggers, runner, logger)

	var wg sync.WaitGroup
	for name, connect := range map[string]func(...dbus.ConnOption) (*dbus.Conn, error){
		"system":  dbus.ConnectSystemBus,
		"session": dbus.ConnectSessionBus,
	} {
		onBus := slices.DeleteFunc(slices.Clone(triggers), func(t Trigger) bool { return t.Bus != name })
		if len(onBus) == 0 {
			continue
		}
		w := &busWatcher{name: name, connect: connect, triggers: onBus, events: events, log: logger}
		wg.Add(1)
		go func() { defer wg.Done(); w.run(ctx) }()
	}

	logger.Info("generic D-Bus trigger daemon running", "dry_run", *dryRun)
	<-ctx.Done()
	wg.Wait()
	<-dispatcherDone
	runner.wait()
	logger.Info("stopped")
}
