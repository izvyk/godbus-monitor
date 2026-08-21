package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type Trigger struct {
	Name          string   `json:"name"`
	Bus           string   `json:"bus"`
	Sender        string   `json:"sender,omitempty"`
	Path          string   `json:"path,omitempty"`
	Interface     string   `json:"interface"`
	Property      string   `json:"property"`
	Operator      string   `json:"operator"`
	ExpectedValue string   `json:"expected_value"`
	Argv          []string `json:"argv"`
	DebounceMs    int      `json:"debounce_ms"`
	TimeoutSec    int      `json:"timeout_sec"`
}

const (
	defaultDebounceMs = 250
	defaultTimeoutSec = 30
	maxDebounceMs     = 24 * 60 * 60 * 1000
	maxTimeoutSec     = 24 * 60 * 60
)

var validOperators = map[string]bool{"==": true, "!=": true, ">": true, "<": true}

func loadConfig(path string) ([]Trigger, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config is not a regular file: %s", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var triggers []Trigger
	if err := dec.Decode(&triggers); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parsing config: trailing JSON data")
		}
		return nil, fmt.Errorf("parsing config: trailing data: %w", err)
	}

	seen := make(map[string]bool, len(triggers))
	for i := range triggers {
		t := &triggers[i]
		if t.Name == "" {
			t.Name = fmt.Sprintf("trigger-%d", i)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("trigger %q: duplicate name", t.Name)
		}
		seen[t.Name] = true
		if t.Bus != "system" && t.Bus != "session" {
			return nil, fmt.Errorf("trigger %q: bus must be system or session", t.Name)
		}
		if t.Interface == "" || t.Property == "" {
			return nil, fmt.Errorf("trigger %q: interface and property are required", t.Name)
		}
		if t.Sender != "" && strings.ContainsAny(t.Sender, " \t\r\n") {
			return nil, fmt.Errorf("trigger %q: invalid sender", t.Name)
		}
		if t.Path != "" && (t.Path[0] != '/' || strings.ContainsAny(t.Path, " \t\r\n")) {
			return nil, fmt.Errorf("trigger %q: invalid object path", t.Name)
		}
		if !validOperators[t.Operator] {
			return nil, fmt.Errorf("trigger %q: invalid operator %q", t.Name, t.Operator)
		}
		if len(t.Argv) == 0 || t.Argv[0] == "" {
			return nil, fmt.Errorf("trigger %q: argv must be non-empty", t.Name)
		}
		if t.DebounceMs == 0 {
			t.DebounceMs = defaultDebounceMs
		}
		if t.TimeoutSec == 0 {
			t.TimeoutSec = defaultTimeoutSec
		}
		if t.DebounceMs < 1 || t.DebounceMs > maxDebounceMs {
			return nil, fmt.Errorf("trigger %q: debounce_ms must be between 1 and %d", t.Name, maxDebounceMs)
		}
		if t.TimeoutSec < 1 || t.TimeoutSec > maxTimeoutSec {
			return nil, fmt.Errorf("trigger %q: timeout_sec must be between 1 and %d", t.Name, maxTimeoutSec)
		}
	}
	return triggers, nil
}
