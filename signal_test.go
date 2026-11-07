package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	signalTestInterface = "org.example.SignalTests"
	signalTestPath      = dbus.ObjectPath("/org/example/SignalTests")
	signalTestName      = signalTestInterface + ".StateChanged"
	propertySignalName  = "org.freedesktop.DBus.Properties.PropertiesChanged"
	signalTestTimeout   = 5 * time.Second
)

func signalTestLoadConfig(t *testing.T, body string) ([]Trigger, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "triggers.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadConfig(path)
}

func TestSignalConfigValidation(t *testing.T) {
	base := map[string]any{
		"bus": "session", "interface": signalTestInterface, "signal": "StateChanged",
		"operator": "==", "expected_value": "true", "argv": []string{"/bin/true"},
	}
	cases := []struct {
		name    string
		changes map[string]any
		remove  []string
		valid   bool
	}{
		{name: "signal defaults", valid: true},
		{name: "explicit zero argument", changes: map[string]any{"argument": 0}, valid: true},
		{name: "nonzero argument", changes: map[string]any{"argument": 2}, valid: true},
		{name: "system bus and optional filters", changes: map[string]any{"bus": "system", "sender": "org.example.Source", "path": string(signalTestPath), "only_on_change": true}, valid: true},
		{name: "system bus without sender", changes: map[string]any{"bus": "system"}},
		{name: "property selector", changes: map[string]any{"property": "Active"}, remove: []string{"signal"}, valid: true},
		{name: "empty property with signal", changes: map[string]any{"property": ""}, valid: true},
		{name: "empty signal with property", changes: map[string]any{"signal": "", "property": "Active"}, valid: true},
		{name: "both selectors", changes: map[string]any{"property": "Active"}},
		{name: "neither selector", remove: []string{"signal"}},
		{name: "both selectors empty", changes: map[string]any{"signal": "", "property": ""}},
		{name: "missing interface", remove: []string{"interface"}},
		{name: "empty interface", changes: map[string]any{"interface": ""}},
		{name: "negative signal argument", changes: map[string]any{"argument": -1}},
		{name: "negative property argument", changes: map[string]any{"argument": -1, "property": "Active"}, remove: []string{"signal"}},
		{name: "fractional argument", changes: map[string]any{"argument": 0.5}},
		{name: "string argument", changes: map[string]any{"argument": "1"}},
		{name: "missing bus", remove: []string{"bus"}},
		{name: "invalid bus", changes: map[string]any{"bus": "other"}},
		{name: "undotted interface", changes: map[string]any{"interface": "notdotted"}},
		{name: "match rule injection", changes: map[string]any{"interface": "org.example',member='X"}},
		{name: "invalid signal member", changes: map[string]any{"signal": "State.Changed"}},
		{name: "unique sender", changes: map[string]any{"sender": ":1.42"}, valid: true},
		{name: "hyphenated sender", changes: map[string]any{"sender": "org.example-app.Source"}, valid: true},
		{name: "invalid sender", changes: map[string]any{"sender": "org.example. Source"}},
		{name: "undotted sender", changes: map[string]any{"sender": "notdotted"}},
		{name: "root path", changes: map[string]any{"path": "/"}, valid: true},
		{name: "relative path", changes: map[string]any{"path": "relative"}},
		{name: "path whitespace", changes: map[string]any{"path": "/bad path"}},
		{name: "path trailing slash", changes: map[string]any{"path": "/bad/"}},
		{name: "missing operator", remove: []string{"operator"}},
		{name: "invalid operator", changes: map[string]any{"operator": ">="}},
		{name: "numeric ordering", changes: map[string]any{"operator": ">", "expected_value": "0.5"}, valid: true},
		{name: "ordering against non-number", changes: map[string]any{"operator": "<", "expected_value": "high"}},
		{name: "missing argv", remove: []string{"argv"}},
		{name: "empty argv", changes: map[string]any{"argv": []string{}}},
		{name: "empty executable", changes: map[string]any{"argv": []string{""}}},
		{name: "negative debounce", changes: map[string]any{"debounce_ms": -1}},
		{name: "oversized debounce", changes: map[string]any{"debounce_ms": maxDebounceMs + 1}},
		{name: "negative timeout", changes: map[string]any{"timeout_sec": -1}},
		{name: "oversized timeout", changes: map[string]any{"timeout_sec": maxTimeoutSec + 1}},
		{name: "unknown field", changes: map[string]any{"argment": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := make(map[string]any, len(base))
			for key, value := range base {
				config[key] = value
			}
			for key, value := range tc.changes {
				config[key] = value
			}
			for _, key := range tc.remove {
				delete(config, key)
			}
			body, err := json.Marshal([]map[string]any{config})
			if err != nil {
				t.Fatal(err)
			}
			triggers, err := signalTestLoadConfig(t, string(body))
			if !tc.valid {
				if err == nil {
					t.Fatalf("accepted invalid config: %s", body)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if len(triggers) != 1 {
				t.Fatalf("got %d triggers, want 1", len(triggers))
			}
			tr := triggers[0]
			if tr.Name != "trigger-0" || tr.DebounceMs != defaultDebounceMs || tr.TimeoutSec != defaultTimeoutSec {
				t.Fatalf("incorrect defaults: %+v", tr)
			}
			wantArgument, _ := config["argument"].(int)
			wantSignal, _ := config["signal"].(string)
			wantProperty, _ := config["property"].(string)
			if tr.Argument != wantArgument || tr.Signal != wantSignal || tr.Property != wantProperty {
				t.Fatalf("selectors/argument not decoded: %+v", tr)
			}
			if want, _ := config["only_on_change"].(bool); tr.OnlyOnChange != want {
				t.Fatalf("only_on_change = %v, want %v", tr.OnlyOnChange, want)
			}
		})
	}
}

func TestSignalConfigDocumentValidation(t *testing.T) {
	valid := `{"name":"same","bus":"session","interface":"org.example.SignalTests","signal":"StateChanged","operator":"==","argv":["/bin/true"]}`
	for name, body := range map[string]string{
		"duplicate names":  "[" + valid + "," + valid + "]",
		"trailing JSON":    "[" + valid + "] {}",
		"trailing junk":    "[" + valid + "] junk",
		"malformed JSON":   "[" + valid,
		"object not array": valid,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := signalTestLoadConfig(t, body); err == nil {
				t.Fatalf("accepted invalid document: %s", body)
			}
		})
	}
}

func signalTestTrigger(t *testing.T, name string) (Trigger, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "marker")
	return Trigger{
		Name: name, Bus: "session", Interface: signalTestInterface, Signal: "StateChanged",
		Operator: "==", ExpectedValue: "true", DebounceMs: 1, TimeoutSec: 2,
		// The marker is passed as an argument, never interpolated into shell source.
		Argv: []string{"/bin/sh", "-c", `printf 'run\n' >> "$1"`, "signal-test", marker},
	}, marker
}

func signalTestEvent(body ...any) wrappedSignal {
	return wrappedSignal{Bus: "session", Signal: &dbus.Signal{
		Name: signalTestName, Path: signalTestPath, Sender: ":1.42", Body: body,
	}}
}

func signalTestPropertyEvent(value any) wrappedSignal {
	e := signalTestEvent(signalTestInterface, map[string]dbus.Variant{"Active": dbus.MakeVariant(value)}, []string{})
	e.Signal.Name = propertySignalName
	return e
}

func signalTestWaitRunner(t *testing.T, runner *scriptRunner) {
	t.Helper()
	done := make(chan struct{})
	go func() { runner.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(signalTestTimeout):
		t.Fatal("runner did not finish within timeout")
	}
}

func signalTestMarkerCount(t *testing.T, marker string, want int) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "run\n"); got != want || string(data) != strings.Repeat("run\n", want) {
		t.Fatalf("marker = %q, want exactly %d runs", data, want)
	}
}

// An unbuffered nil event fences the preceding event: the dispatcher cannot
// receive it until handleSignal has returned and registered any runner work.
func signalTestDispatcher(t *testing.T, triggers []Trigger) func(wrappedSignal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	runner := newScriptRunner()
	events := make(chan wrappedSignal)
	startDispatcher(ctx, events, triggers, runner, testLogger())
	t.Cleanup(func() {
		cancel()
		signalTestWaitRunner(t, runner)
	})
	return func(e wrappedSignal) {
		t.Helper()
		for _, event := range []wrappedSignal{e, {}} {
			select {
			case events <- event:
			case <-time.After(signalTestTimeout):
				t.Fatal("dispatcher did not drain events")
			}
		}
		signalTestWaitRunner(t, runner)
	}
}

func TestSignalDispatcherFiltersAndMalformed(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Trigger, *wrappedSignal)
		want   int
	}{
		{name: "matching", want: 1},
		{name: "optional filters match", want: 1, change: func(tr *Trigger, e *wrappedSignal) { tr.Sender = e.Signal.Sender; tr.Path = string(e.Signal.Path) }},
		{name: "nil signal", change: func(_ *Trigger, e *wrappedSignal) { e.Signal = nil }},
		{name: "nil body", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Body = nil }},
		{name: "empty body", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Body = []any{} }},
		{name: "wrong bus", change: func(_ *Trigger, e *wrappedSignal) { e.Bus = "system" }},
		{name: "wrong interface", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Name = "org.example.Other.StateChanged" }},
		{name: "wrong member", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Name = signalTestInterface + ".Other" }},
		{name: "member alone", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Name = "StateChanged" }},
		{name: "name prefix only", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Name += "Extra" }},
		{name: "empty name", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Name = "" }},
		{name: "wrong path", change: func(tr *Trigger, _ *wrappedSignal) { tr.Path = "/other" }},
		{name: "wrong unique sender", change: func(tr *Trigger, _ *wrappedSignal) { tr.Sender = ":1.99" }},
		{name: "false condition", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Body = []any{false} }},
		{name: "unsupported value", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Body = []any{[]string{"true"}} }},
		{name: "nil argument", change: func(_ *Trigger, e *wrappedSignal) { e.Signal.Body = []any{nil} }},
		{name: "bad expected boolean", change: func(tr *Trigger, _ *wrappedSignal) { tr.ExpectedValue = "not-bool" }},
		{name: "bad operator", change: func(tr *Trigger, _ *wrappedSignal) { tr.Operator = ">=" }},
		{name: "missing selected argument", change: func(tr *Trigger, _ *wrappedSignal) { tr.Argument = 1 }},
		{name: "far out of range argument", change: func(tr *Trigger, _ *wrappedSignal) { tr.Argument = int(^uint(0) >> 1) }},
		{name: "property signal not regular signal", change: func(_ *Trigger, e *wrappedSignal) { *e = signalTestPropertyEvent(true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, marker := signalTestTrigger(t, "regular")
			e := signalTestEvent(true)
			if tc.change != nil {
				tc.change(&tr, &e)
			}
			dispatch := signalTestDispatcher(t, []Trigger{tr})
			dispatch(e)
			signalTestMarkerCount(t, marker, tc.want)
		})
	}
}

func TestSignalDispatcherSelectedArgumentAndEvaluator(t *testing.T) {
	cases := []struct {
		name     string
		argument int
		body     []any
		op       string
		expected string
		want     int
	}{
		{"default argument zero", 0, []any{true, false}, "==", "true", 1},
		{"second argument", 1, []any{false, true}, "==", "true", 1},
		{"first argument must not substitute", 1, []any{true, false}, "==", "true", 0},
		{"third argument", 2, []any{"ignored", false, true}, "==", "true", 1},
		{"selected variant", 1, []any{false, dbus.MakeVariant(true)}, "==", "true", 1},
		{"nested variants", 0, []any{dbus.MakeVariant(dbus.MakeVariant(true))}, "==", "true", 1},
		{"numeric comparison", 1, []any{int32(0), uint32(7)}, ">", "5", 1},
		{"string comparison", 1, []any{false, "ready"}, "==", "ready", 1},
		{"boolean inequality", 0, []any{false}, "!=", "true", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, marker := signalTestTrigger(t, "selected")
			tr.Argument, tr.Operator, tr.ExpectedValue = tc.argument, tc.op, tc.expected
			dispatch := signalTestDispatcher(t, []Trigger{tr})
			dispatch(signalTestEvent(tc.body...))
			signalTestMarkerCount(t, marker, tc.want)
		})
	}
}

func TestSignalDispatcherValueTransitions(t *testing.T) {
	for _, property := range []bool{false, true} {
		for _, onlyOnChange := range []bool{false, true} {
			t.Run(fmt.Sprintf("property=%v/only_on_change=%v", property, onlyOnChange), func(t *testing.T) {
				tr, marker := signalTestTrigger(t, "transitions")
				tr.OnlyOnChange = onlyOnChange
				if property {
					tr.Signal, tr.Property = "", "Active"
				} else {
					tr.Argument = 1
				}
				dispatch := signalTestDispatcher(t, []Trigger{tr})
				count := 0
				for i, value := range []bool{true, true, false, true, true} {
					var event wrappedSignal
					if property {
						event = signalTestPropertyEvent(value)
					} else {
						// Changing an unselected argument must not count as a change.
						event = signalTestEvent(int32(i), dbus.MakeVariant(value))
					}
					dispatch(event)
					if value && (!onlyOnChange || i == 0 || i == 3) {
						count++
					}
					signalTestMarkerCount(t, marker, count)
				}
			})
		}
	}
}

func TestSignalDispatcherTracksTriggersIndependently(t *testing.T) {
	first, firstMarker := signalTestTrigger(t, "first")
	second, secondMarker := signalTestTrigger(t, "second")
	first.OnlyOnChange, second.OnlyOnChange = true, true
	second.Argument = 1
	dispatch := signalTestDispatcher(t, []Trigger{first, second})
	dispatch(signalTestEvent(true, false))
	signalTestMarkerCount(t, firstMarker, 1)
	signalTestMarkerCount(t, secondMarker, 0)
	dispatch(signalTestEvent(true, true))
	signalTestMarkerCount(t, firstMarker, 1)
	signalTestMarkerCount(t, secondMarker, 1)
	dispatch(signalTestEvent(true, true))
	signalTestMarkerCount(t, firstMarker, 1)
	signalTestMarkerCount(t, secondMarker, 1)
}

func TestSignalDispatcherMixedModes(t *testing.T) {
	regular, regularMarker := signalTestTrigger(t, "regular")
	property, propertyMarker := signalTestTrigger(t, "property")
	property.Signal, property.Property = "", "Active"
	dispatch := signalTestDispatcher(t, []Trigger{regular, property})
	dispatch(signalTestEvent(true))
	signalTestMarkerCount(t, regularMarker, 1)
	signalTestMarkerCount(t, propertyMarker, 0)
	dispatch(signalTestPropertyEvent(true))
	signalTestMarkerCount(t, regularMarker, 1)
	signalTestMarkerCount(t, propertyMarker, 1)
	shaped := signalTestPropertyEvent(true)
	shaped.Signal.Name = signalTestName
	dispatch(shaped)
	signalTestMarkerCount(t, regularMarker, 1)
	signalTestMarkerCount(t, propertyMarker, 1)
}

func TestSignalDispatcherIgnoredEventsDoNotChangeTracker(t *testing.T) {
	for _, property := range []bool{false, true} {
		t.Run(fmt.Sprintf("property=%v", property), func(t *testing.T) {
			tr, marker := signalTestTrigger(t, "filtered-transitions")
			tr.OnlyOnChange, tr.Path, tr.Sender = true, string(signalTestPath), ":1.42"
			makeEvent := func(value bool) wrappedSignal { return signalTestEvent(value) }
			if property {
				tr.Signal, tr.Property = "", "Active"
				makeEvent = func(value bool) wrappedSignal { return signalTestPropertyEvent(value) }
			}
			dispatch := signalTestDispatcher(t, []Trigger{tr})
			dispatch(makeEvent(true))
			for _, change := range []func(*wrappedSignal){
				func(e *wrappedSignal) { e.Bus = "system" },
				func(e *wrappedSignal) { e.Signal.Path = "/other" },
				func(e *wrappedSignal) { e.Signal.Sender = ":1.99" },
				func(e *wrappedSignal) { e.Signal.Name = "org.example.Other.StateChanged" },
				func(e *wrappedSignal) { e.Signal.Body = nil },
			} {
				event := makeEvent(false)
				change(&event)
				dispatch(event)
				dispatch(makeEvent(true))
				signalTestMarkerCount(t, marker, 1)
			}
		})
	}
}

func TestSignalPropertyDispatcher(t *testing.T) {
	cases := []struct {
		name   string
		change func(*wrappedSignal)
		want   int
	}{
		{name: "valid properties changed", want: 1},
		{name: "two element property body", want: 1, change: func(e *wrappedSignal) { e.Signal.Body = e.Signal.Body[:2] }},
		{name: "unrelated invalidation ignored", want: 1, change: func(e *wrappedSignal) { e.Signal.Body[2] = []string{"Other"} }},
		{name: "changed value wins over invalidation", want: 1, change: func(e *wrappedSignal) { e.Signal.Body[2] = []string{"Active"} }},
		{name: "nil signal", change: func(e *wrappedSignal) { e.Signal = nil }},
		{name: "nil body", change: func(e *wrappedSignal) { e.Signal.Body = nil }},
		{name: "short body", change: func(e *wrappedSignal) { e.Signal.Body = e.Signal.Body[:1] }},
		{name: "wrong signal interface", change: func(e *wrappedSignal) { e.Signal.Name = "org.example.Other.PropertiesChanged" }},
		{name: "wrong signal member", change: func(e *wrappedSignal) { e.Signal.Name = "org.freedesktop.DBus.Properties.Other" }},
		{name: "empty signal name", change: func(e *wrappedSignal) { e.Signal.Name = "" }},
		{name: "regular signal with property shaped body", change: func(e *wrappedSignal) { e.Signal.Name = signalTestName }},
		{name: "wrong property interface", change: func(e *wrappedSignal) { e.Signal.Body[0] = "org.example.Other" }},
		{name: "nonstring interface", change: func(e *wrappedSignal) { e.Signal.Body[0] = true }},
		{name: "variant interface is malformed", change: func(e *wrappedSignal) { e.Signal.Body[0] = dbus.MakeVariant(signalTestInterface) }},
		{name: "wrong changed map type", change: func(e *wrappedSignal) { e.Signal.Body[1] = map[string]any{"Active": true} }},
		{name: "nil changed value", change: func(e *wrappedSignal) { e.Signal.Body[1] = nil }},
		{name: "typed nil changed map", change: func(e *wrappedSignal) { e.Signal.Body[1] = map[string]dbus.Variant(nil) }},
		{name: "other property", change: func(e *wrappedSignal) { e.Signal.Body[1] = map[string]dbus.Variant{"Other": dbus.MakeVariant(true)} }},
		{name: "false property", change: func(e *wrappedSignal) { e.Signal.Body[1] = map[string]dbus.Variant{"Active": dbus.MakeVariant(false)} }},
		{name: "malformed invalidation", change: func(e *wrappedSignal) { e.Signal.Body[1] = map[string]dbus.Variant{}; e.Signal.Body[2] = "Active" }},
		{name: "wrong bus", change: func(e *wrappedSignal) { e.Bus = "system" }},
		{name: "wrong path", change: func(e *wrappedSignal) { e.Signal.Path = "/other" }},
		{name: "wrong sender", change: func(e *wrappedSignal) { e.Signal.Sender = ":1.99" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, marker := signalTestTrigger(t, "property")
			tr.Signal, tr.Property = "", "Active"
			tr.Path, tr.Sender = string(signalTestPath), ":1.42"
			e := signalTestPropertyEvent(true)
			if tc.change != nil {
				tc.change(&e)
			}
			dispatch := signalTestDispatcher(t, []Trigger{tr})
			dispatch(e)
			signalTestMarkerCount(t, marker, tc.want)
		})
	}
}

func signalTestPrivateBus(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("private bus integration requires dbus-daemon")
	}
	// Never consult DBUS_SESSION_BUS_ADDRESS or connect to the host's buses. The
	// config is ours too: --session needs /etc/dbus-1, absent in the Nix sandbox.
	dir := t.TempDir()
	address := "unix:path=" + filepath.Join(dir, "bus")
	config := filepath.Join(dir, "bus.conf")
	body := `<busconfig><listen>` + address + `</listen><policy context="default"><allow send_destination="*"/><allow eavesdrop="true"/><allow own="*"/></policy></busconfig>`
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config-file="+config, "--nofork", "--nopidfile", "--print-address=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start private dbus-daemon: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(signalTestTimeout):
			t.Error("private dbus-daemon did not exit")
		}
	})
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
	}()
	select {
	case printed := <-ready:
		if !strings.HasPrefix(printed, address) {
			t.Fatalf("private daemon did not publish requested address: %q", printed)
		}
		return printed
	case <-time.After(signalTestTimeout):
		t.Fatal("private dbus-daemon startup timed out")
		return ""
	}
}

func signalTestConnect(t *testing.T, address string) *dbus.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	conn, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		cancel()
		t.Fatalf("connect to private bus: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(); cancel() })
	return conn
}

type signalTestReadyHandler struct {
	slog.Handler
	ready chan struct{}
	once  sync.Once
}

func (h *signalTestReadyHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "watching bus" {
		h.once.Do(func() { close(h.ready) })
	}
	return h.Handler.Handle(ctx, record)
}

func signalTestStartWatcher(t *testing.T, conn *dbus.Conn, triggers []Trigger) (<-chan wrappedSignal, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	events := make(chan wrappedSignal, 32)
	handler := &signalTestReadyHandler{Handler: testLogger().Handler(), ready: make(chan struct{})}
	watcher := &busWatcher{name: "session", triggers: triggers, events: events, log: slog.New(handler)}
	done := make(chan struct{})
	go func() { _ = watcher.watch(ctx, conn); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(signalTestTimeout):
			t.Error("watcher did not stop within timeout")
		}
	}
	t.Cleanup(stop)
	select {
	case <-handler.ready:
	case <-done:
		t.Fatal("watcher exited before subscribing")
	case <-time.After(signalTestTimeout):
		t.Fatal("watcher subscription setup timed out")
	}
	return events, stop
}

func signalTestReceive(t *testing.T, events <-chan wrappedSignal, name string) wrappedSignal {
	t.Helper()
	select {
	case event := <-events:
		if event.Bus != "session" || event.Signal == nil || event.Signal.Name != name {
			t.Fatalf("unexpected watcher event: %+v; want %s on session bus", event, name)
		}
		return event
	case <-time.After(signalTestTimeout):
		t.Fatalf("watcher did not deliver %s", name)
		return wrappedSignal{}
	}
}

func signalTestEmit(t *testing.T, conn *dbus.Conn, name string, body ...any) {
	t.Helper()
	if err := conn.Emit(signalTestPath, name, body...); err != nil {
		t.Fatalf("emit %s on private bus: %v", name, err)
	}
}

func TestGNOMEActiveChangedPrivateBus(t *testing.T) {
	address := signalTestPrivateBus(t)
	monitor := signalTestConnect(t, address)
	emitter := signalTestConnect(t, address)
	tr, marker := signalTestTrigger(t, "suspend-on-screen-blank")
	tr.Sender = "org.gnome.ScreenSaver"
	tr.Path = "/org/gnome/ScreenSaver"
	tr.Interface = "org.gnome.ScreenSaver"
	tr.Signal = "ActiveChanged"
	tr.OnlyOnChange = true
	// Validate the GNOME config, substituting only a harmless marker command.
	body, err := json.Marshal([]Trigger{tr})
	if err != nil {
		t.Fatal(err)
	}
	triggers, err := signalTestLoadConfig(t, string(body))
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := emitter.RequestName(tr.Sender, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("acquire private GNOME test name: reply=%v err=%v", reply, err)
	}
	events, stop := signalTestStartWatcher(t, monitor, triggers)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), signalTestTimeout)
	defer cancel()
	runner := newScriptRunner()
	tracker := newValueTracker()
	for i, value := range []bool{true, true, false, true} {
		name := tr.Interface + "." + tr.Signal
		if err := emitter.Emit(dbus.ObjectPath(tr.Path), name, value); err != nil {
			t.Fatal(err)
		}
		handleSignal(ctx, signalTestReceive(t, events, name), triggers, runner, tracker, testLogger())
		signalTestWaitRunner(t, runner)
		want := 1
		if i == 3 {
			want = 2
		}
		signalTestMarkerCount(t, marker, want)
	}
}

func TestSignalPrivateBusWatcherSubscriptions(t *testing.T) {
	address := signalTestPrivateBus(t)
	monitor := signalTestConnect(t, address)
	emitter := signalTestConnect(t, address)
	tr, _ := signalTestTrigger(t, "regular")
	property := tr
	property.Name, property.Signal, property.Property = "property", "", "Active"
	propertyDuplicate := property
	propertyDuplicate.Name, propertyDuplicate.Property = "property-duplicate", "Other"
	regularDuplicate := tr
	regularDuplicate.Name, regularDuplicate.Argument = "regular-duplicate", 1
	otherMember := tr
	otherMember.Name, otherMember.Signal = "other-member", "OtherChanged"
	regularOnly := tr
	regularOnly.Name, regularOnly.Interface = "regular-only", "org.example.RegularOnly"
	events, _ := signalTestStartWatcher(t, monitor, []Trigger{property, propertyDuplicate, tr, regularDuplicate, otherMember, regularOnly})
	// Non-matching traffic precedes a subscribed fence on the same connection.
	// D-Bus preserves message order, so no sleeps are needed to assert exclusion.
	// The strict receive order also proves triggers sharing a rule see one copy.
	signalTestEmit(t, emitter, "org.example.Unwatched.StateChanged", true)
	signalTestEmit(t, emitter, signalTestInterface+".Unwatched", true)
	signalTestEmit(t, emitter, propertySignalName, "org.example.RegularOnly", map[string]dbus.Variant{"Active": dbus.MakeVariant(true)}, []string{})
	signalTestEmit(t, emitter, signalTestName, true)
	signalTestReceive(t, events, signalTestName)
	signalTestEmit(t, emitter, propertySignalName, signalTestInterface, map[string]dbus.Variant{"Active": dbus.MakeVariant(true)}, []string{})
	signalTestReceive(t, events, propertySignalName)
	signalTestEmit(t, emitter, signalTestInterface+".OtherChanged", false)
	signalTestReceive(t, events, signalTestInterface+".OtherChanged")
	signalTestEmit(t, emitter, "org.example.RegularOnly.StateChanged", true)
	signalTestReceive(t, events, "org.example.RegularOnly.StateChanged")
}

func TestSignalPrivateBusWatcherCancellationWithBackpressure(t *testing.T) {
	address := signalTestPrivateBus(t)
	monitor := signalTestConnect(t, address)
	emitter := signalTestConnect(t, address)
	tr, _ := signalTestTrigger(t, "backpressure")
	events, stop := signalTestStartWatcher(t, monitor, []Trigger{tr})
	for i := 0; i < cap(events)+8; i++ {
		signalTestEmit(t, emitter, signalTestName, true)
	}
	deadline := time.NewTimer(signalTestTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for len(events) < cap(events) {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("watcher did not fill its downstream channel")
		}
	}
	// Cancellation must unblock the forwarding select even without a dispatcher.
	stop()
}

// A bus that accepts connections but rejects a match rule must not make the
// daemon spin: loadConfig catches malformed names, but not e.g. a bus policy.
func TestSignalPrivateBusWatcherBacksOffOnRejectedMatchRule(t *testing.T) {
	address := signalTestPrivateBus(t)
	tr, _ := signalTestTrigger(t, "rejected")
	tr.Interface = "notdotted"
	var connects atomic.Int32
	watcher := &busWatcher{
		name: "session", triggers: []Trigger{tr}, events: make(chan wrappedSignal), log: testLogger(),
		connect: func(opts ...dbus.ConnOption) (*dbus.Conn, error) {
			connects.Add(1)
			return dbus.Connect(address, opts...)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), minBackoff/2)
	defer cancel()
	watcher.run(ctx)
	if n := connects.Load(); n != 1 {
		t.Fatalf("connected %d times within the first backoff window, want 1", n)
	}
}

func TestSignalPrivateBusWatcherReconnects(t *testing.T) {
	address := signalTestPrivateBus(t)
	emitter := signalTestConnect(t, address)
	tr, _ := signalTestTrigger(t, "reconnect")
	events := make(chan wrappedSignal, 32)
	watcher := &busWatcher{
		name: "session", triggers: []Trigger{tr}, events: events, log: testLogger(),
		connect: func(opts ...dbus.ConnOption) (*dbus.Conn, error) { return dbus.Connect(address, opts...) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan struct{})
	go func() { watcher.run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(signalTestTimeout):
			t.Error("watcher did not stop after cancellation")
		}
	})
	// Subscribing is asynchronous here, so keep emitting until a signal arrives.
	receive := func() *dbus.Conn {
		t.Helper()
		deadline := time.After(signalTestTimeout)
		for {
			signalTestEmit(t, emitter, signalTestName, true)
			select {
			case e := <-events:
				return e.Conn
			case <-time.After(20 * time.Millisecond):
			case <-deadline:
				t.Fatal("no signal delivered")
			}
		}
	}
	first := receive()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	for receive() == first {
	}
}

type signalTestPropertyProvider struct{}

func (*signalTestPropertyProvider) Get(iface, property string) (dbus.Variant, *dbus.Error) {
	if iface != signalTestInterface || property != "Active" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{"unknown test property"})
	}
	return dbus.MakeVariant(true), nil
}

func TestSignalPrivateBusWellKnownSenderBothModes(t *testing.T) {
	address := signalTestPrivateBus(t)
	monitor := signalTestConnect(t, address)
	emitter := signalTestConnect(t, address)
	other := signalTestConnect(t, address)
	if err := emitter.Export(&signalTestPropertyProvider{}, signalTestPath, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatalf("export private property provider: %v", err)
	}
	const sender = "org.example.SignalTestSource"
	if reply, err := emitter.RequestName(sender, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("acquire private well-known name: reply=%v err=%v", reply, err)
	}
	regular, regularMarker := signalTestTrigger(t, "regular")
	regular.Sender, regular.Path, regular.Argument = sender, string(signalTestPath), 1
	property, propertyMarker := signalTestTrigger(t, "property")
	property.Signal, property.Property, property.Sender, property.Path = "", "Active", sender, string(signalTestPath)
	absent, absentMarker := signalTestTrigger(t, "absent")
	absent.Sender, absent.Argument = "org.example.Absent", 1
	triggers := []Trigger{regular, property, absent}
	events, stop := signalTestStartWatcher(t, monitor, triggers)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runner := newScriptRunner()
	t.Cleanup(func() { cancel(); signalTestWaitRunner(t, runner) })
	tracker := newValueTracker()
	dispatch := func(name string) {
		event := signalTestReceive(t, events, name)
		handleSignal(ctx, event, triggers, runner, tracker, testLogger())
		signalTestWaitRunner(t, runner)
	}
	for _, source := range []*dbus.Conn{other, emitter} {
		signalTestEmit(t, source, signalTestName, false, true)
		dispatch(signalTestName)
		want := 0
		if source == emitter {
			want = 1
		}
		signalTestMarkerCount(t, regularMarker, want)
		signalTestMarkerCount(t, propertyMarker, 0)
	}
	for _, source := range []*dbus.Conn{other, emitter} {
		signalTestEmit(t, source, propertySignalName, signalTestInterface, map[string]dbus.Variant{"Active": dbus.MakeVariant(true)}, []string{})
		dispatch(propertySignalName)
		want := 0
		if source == emitter {
			want = 1
		}
		signalTestMarkerCount(t, propertyMarker, want)
		signalTestMarkerCount(t, regularMarker, 1)
	}
	// An invalidated property is fetched with Properties.Get rather than
	// treating the absent changed value as false.
	signalTestEmit(t, emitter, propertySignalName, signalTestInterface, map[string]dbus.Variant{}, []string{"Active"})
	dispatch(propertySignalName)
	signalTestMarkerCount(t, propertyMarker, 2)
	signalTestMarkerCount(t, regularMarker, 1)
	// Resolve the current owner, not a stale owner cached across signals.
	if reply, err := emitter.ReleaseName(sender); err != nil || reply != dbus.ReleaseNameReplyReleased {
		t.Fatalf("release private name: reply=%v err=%v", reply, err)
	}
	if reply, err := other.RequestName(sender, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("transfer private name: reply=%v err=%v", reply, err)
	}
	signalTestEmit(t, emitter, signalTestName, false, true)
	dispatch(signalTestName)
	signalTestMarkerCount(t, regularMarker, 1)
	signalTestEmit(t, other, signalTestName, false, true)
	dispatch(signalTestName)
	signalTestMarkerCount(t, regularMarker, 2)
	signalTestEmit(t, emitter, propertySignalName, signalTestInterface, map[string]dbus.Variant{"Active": dbus.MakeVariant(true)}, []string{})
	dispatch(propertySignalName)
	signalTestMarkerCount(t, propertyMarker, 2)
	signalTestEmit(t, other, propertySignalName, signalTestInterface, map[string]dbus.Variant{"Active": dbus.MakeVariant(true)}, []string{})
	dispatch(propertySignalName)
	signalTestMarkerCount(t, propertyMarker, 3)
	signalTestMarkerCount(t, absentMarker, 0)
	stop()
}
