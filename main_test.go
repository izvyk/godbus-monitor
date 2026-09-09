package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------- evaluateCondition ----------

func TestEvaluateCondition(t *testing.T) {
	cases := []struct {
		name     string
		actual   any
		op       string
		expected string
		want     bool
	}{
		{"string eq", "hello", "==", "hello", true},
		{"string ne", "hello", "!=", "world", true},
		{"string gt unsupported", "b", ">", "a", false},
		{"bool eq", true, "==", "true", true},
		{"bool ne", true, "!=", "false", true},
		{"bool bad expected", true, "==", "notabool", false},
		{"int32 eq", int32(1), "==", "1", true},
		{"int32 gt", int32(5), ">", "3", true},
		{"int32 lt", int32(5), "<", "3", false},
		{"uint64 gt", uint64(10), ">", "2", true},
		{"float lt", 1.5, "<", "2.0", true},
		{"float eq", 2.5, "==", "2.5", true},
		{"int16 eq", int16(-3), "==", "-3", true},
		{"unsupported type", []string{"a"}, "==", "a", false},
		{"bad operator", 1.0, ">=", "1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evaluateCondition(c.actual, c.op, c.expected); got != c.want {
				t.Fatalf("evaluateCondition(%v, %q, %q) = %v, want %v", c.actual, c.op, c.expected, got, c.want)
			}
		})
	}
}

func TestValueTracker(t *testing.T) {
	tracker := newValueTracker()
	if !tracker.hasChanged("lid", true) {
		t.Fatal("first observation should count as changed")
	}
	if tracker.hasChanged("lid", true) {
		t.Fatal("repeated value should not count as changed")
	}
	if !tracker.hasChanged("lid", false) {
		t.Fatal("transition should count as changed")
	}
	if !tracker.hasChanged("other", false) {
		t.Fatal("values should be tracked independently per trigger")
	}
}

// ---------- cappedBuffer ----------

func TestCappedBufferTruncates(t *testing.T) {
	var b cappedBuffer
	chunk := make([]byte, maxOutputBytes)
	for i := range chunk {
		chunk[i] = 'a'
	}
	b.Write(chunk)
	if b.truncated {
		t.Fatal("should not be truncated after exactly max bytes")
	}
	b.Write([]byte("more"))
	if !b.truncated {
		t.Fatal("expected truncated flag after overflow")
	}
	if len(b.b) != maxOutputBytes {
		t.Fatalf("buffer len = %d, want %d", len(b.b), maxOutputBytes)
	}
	s := b.String()
	if len(s) <= maxOutputBytes || s[len(s)-1] != ']' {
		t.Fatalf("expected truncation marker, got len=%d", len(s))
	}
}

// ---------- config loading ----------

// func TestLoadConfigValid(t *testing.T) {
// 	triggers, err := loadConfig("testdata/valid.json")
// 	if err != nil {
// 		t.Fatalf("loadConfig: %v", err)
// 	}
// 	if len(triggers) != 2 {
// 		t.Fatalf("got %d triggers, want 2", len(triggers))
// 	}
// 	if triggers[0].DebounceMs != defaultDebounceMs {
// 		t.Fatalf("default debounce = %d, want %d", triggers[0].DebounceMs, defaultDebounceMs)
// 	}
// 	if triggers[1].TimeoutSec != defaultTimeoutSec {
// 		t.Fatalf("default timeout = %d, want %d", triggers[1].TimeoutSec, defaultTimeoutSec)
// 	}
// 	if triggers[0].Name != "a" || triggers[1].Name != "b" {
// 		t.Fatalf("unexpected names: %q %q", triggers[0].Name, triggers[1].Name)
// 	}
// }

func TestLoadConfigOnlyOnChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	body := `[{"name":"lid","bus":"system","interface":"org.freedesktop.login1.Manager","property":"LidClosed","operator":"==","expected_value":"true","only_on_change":true,"argv":["true"]}]`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	triggers, err := loadConfig(p)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(triggers) != 1 || !triggers[0].OnlyOnChange {
		t.Fatalf("only_on_change was not loaded: %+v", triggers)
	}
}

func TestLoadConfigRejectsBad(t *testing.T) {
	cases := map[string]string{
		"bad operator":      `[{"bus":"session","interface":"i","property":"p","operator":">=","argv":["true"]}]`,
		"empty argv":        `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":[]}]`,
		"bad bus":           `[{"bus":"session2","interface":"i","property":"p","operator":"==","argv":["true"]}]`,
		"duplicate name":    `[{"name":"x","bus":"session","interface":"i","property":"p","operator":"==","argv":["true"]},{"name":"x","bus":"session","interface":"i","property":"p","operator":"==","argv":["true"]}]`,
		"trailing data":     `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":["true"]}] {}`,
		"unknown field":     `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":["true"],"nope":1}]`,
		"missing property":  `[{"bus":"session","interface":"i","operator":"==","argv":["true"]}]`,
		"bad path":          `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":["true"],"path":"no/slash"}]`,
		"debounce too big":  `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":["true"],"debounce_ms":86400001}]`,
		"timeout too small": `[{"bus":"session","interface":"i","property":"p","operator":"==","argv":["true"],"timeout_sec":-1}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(p); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

// ---------- scriptRunner debounce/pending semantics ----------

func TestRunnerFiresOnce(t *testing.T) {
	r := newScriptRunner()
	log := testLogger()
	tr := Trigger{
		Name: "t1", Bus: "session", Interface: "i", Property: "p",
		Operator: "==", ExpectedValue: "x",
		Argv:       []string{"/bin/sh", "-c", "true"},
		DebounceMs: 50, TimeoutSec: 5,
	}
	ctx := context.Background()
	r.run(ctx, tr, log)
	r.run(ctx, tr, log) // coalesced into pending
	r.wait()
	// If we get here without hanging, the state machine is consistent.
}

func TestRunnerPendingFiresAfterDebounce(t *testing.T) {
	r := newScriptRunner()
	log := testLogger()
	tr := Trigger{
		Name: "t2", Bus: "session", Interface: "i", Property: "p",
		Operator: "==", ExpectedValue: "x",
		Argv:       []string{"/bin/sh", "-c", "sleep 0.05"},
		DebounceMs: 200, TimeoutSec: 5,
	}
	ctx := context.Background()
	start := time.Now()
	r.run(ctx, tr, log) // starts running
	r.run(ctx, tr, log) // sets pending
	r.wait()
	elapsed := time.Since(start)
	// The pending run must have been scheduled after the debounce window,
	// so total wall time should exceed the debounce period.
	if elapsed < 200*time.Millisecond {
		t.Fatalf("pending run fired too early: %v", elapsed)
	}
}

func TestRunnerCancelsOnContextDone(t *testing.T) {
	r := newScriptRunner()
	log := testLogger()
	tr := Trigger{
		Name: "t3", Bus: "session", Interface: "i", Property: "p",
		Operator: "==", ExpectedValue: "x",
		Argv:       []string{"/bin/sh", "-c", "sleep 30"},
		DebounceMs: 10, TimeoutSec: 60,
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.run(ctx, tr, log)
	cancel()
	done := make(chan struct{})
	go func() { r.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not shut down after context cancel")
	}
}

// Two rapid events: the second must not be lost — after debounce it must
// schedule exactly one more run.
func TestRunnerDebouncedEventEventuallyFires(t *testing.T) {
	r := newScriptRunner()
	log := testLogger()
	marker := filepath.Join(t.TempDir(), "count")
	tr := Trigger{
		Name: "t5", Bus: "session", Interface: "i", Property: "p",
		Operator: "==", ExpectedValue: "x",
		Argv:       []string{"/bin/sh", "-c", "echo x >> " + marker},
		DebounceMs: 150, TimeoutSec: 5,
	}
	ctx := context.Background()
	r.run(ctx, tr, log)
	r.run(ctx, tr, log) // debounced -> must still fire later
	r.wait()

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker file missing: %v", err)
	}
	runs := strings.Count(string(data), "\n")
	if runs != 2 {
		t.Fatalf("expected 2 runs (initial + debounced), got %d (content %q)", runs, string(data))
	}
}

func TestDispatcherDrainsEvents(t *testing.T) {
	events := make(chan wrappedSignal, 4)
	runner := newScriptRunner()
	registry := newConnRegistry()
	log := testLogger()
	triggers := []Trigger{{
		Name: "d", Bus: "session", Interface: "org.example.I", Property: "P",
		Operator: "==", ExpectedValue: "1",
		Argv: []string{"/bin/true"}, DebounceMs: 10, TimeoutSec: 5,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDispatcher(ctx, events, triggers, runner, registry, log)

	// Feed a malformed signal (no body) — must not panic or wedge.
	events <- wrappedSignal{Bus: "session", Signal: nil}
	// Feed more than the channel capacity of well-formed but non-matching
	// signals; the dispatcher must keep draining.
	for i := 0; i < 8; i++ {
		events <- wrappedSignal{Bus: "system", Signal: nil}
	}
	// If the dispatcher were blocked, the send above would block; reaching
	// here means it drains. Give it a moment then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	runner.wait()
}

func TestRunnerTimeoutKillsProcess(t *testing.T) {
	r := newScriptRunner()
	log := testLogger()
	tr := Trigger{
		Name: "t4", Bus: "session", Interface: "i", Property: "p",
		Operator: "==", ExpectedValue: "x",
		Argv:       []string{"/bin/sh", "-c", "sleep 30"},
		DebounceMs: 10, TimeoutSec: 1,
	}
	start := time.Now()
	r.run(context.Background(), tr, log)
	r.wait()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout did not kill process: %v", elapsed)
	}
}

// ---------- connRegistry ----------

func TestConnRegistryNilSafe(t *testing.T) {
	r := newConnRegistry()
	if r.get("system") != nil {
		t.Fatal("expected nil for missing bus")
	}
	r.set("system", nil)
	if r.get("system") != nil {
		t.Fatal("expected nil after set(nil)")
	}
	_, err := r.fetchProperty(context.Background(), "system", "s", "/p", "i", "prop")
	if err == nil {
		t.Fatal("expected error when no connection")
	}
}
