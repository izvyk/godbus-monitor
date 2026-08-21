package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// Trigger defines a single D-Bus event rule.
type Trigger struct {
	Name          string `json:"name"`           // optional, used only for logging
	Bus           string `json:"bus"`             // "system" or "session"
	Interface     string `json:"interface"`       // e.g. "org.gnome.Mutter.DisplayConfig"
	Property      string `json:"property"`        // e.g. "PowerSaveMode"
	Operator      string `json:"operator"`        // "==", "!=", ">", "<" (default "==")
	ExpectedValue string `json:"expected_value"`  // stringified value, e.g. "0", "true"
	Script        string `json:"script"`          // sh -c command to run on match
	DebounceMs    int    `json:"debounce_ms"`     // minimum gap between runs of this trigger (default 250ms)
	TimeoutSec    int    `json:"timeout_sec"`     // kill the script if it runs longer than this (default 30s)
}

const (
	defaultDebounceMs = 250
	defaultTimeoutSec = 30
)

var validOperators = map[string]bool{"==": true, "!=": true, ">": true, "<": true, "": true}

// loadConfig reads, parses, validates and fills in defaults for the trigger
// list. It intentionally fails loudly (rather than skipping bad entries) --
// this daemon runs arbitrary shell commands unattended, so a malformed rule
// silently doing nothing (or worse, matching more broadly than intended) is
// worse than refusing to start.
func loadConfig(path string) ([]Trigger, error) {
	if info, err := os.Stat(path); err == nil {
		if mode := info.Mode().Perm(); mode&0o022 != 0 {
			fmt.Fprintf(os.Stderr,
				"warning: %s is group/world-writable (mode %v) -- this file controls what shell commands run automatically on D-Bus events, lock it down (chmod 600)\n",
				path, mode)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var triggers []Trigger
	if err := json.Unmarshal(data, &triggers); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	seen := make(map[string]bool)
	for i := range triggers {
		t := &triggers[i]
		if t.Name == "" {
			t.Name = fmt.Sprintf("trigger-%d", i)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("duplicate trigger name %q (name must be unique, it's used to key debouncing)", t.Name)
		}
		seen[t.Name] = true

		if t.Bus != "system" && t.Bus != "session" {
			return nil, fmt.Errorf("trigger %q: bus must be \"system\" or \"session\", got %q", t.Name, t.Bus)
		}
		if t.Interface == "" {
			return nil, fmt.Errorf("trigger %q: interface must not be empty", t.Name)
		}
		if t.Property == "" {
			return nil, fmt.Errorf("trigger %q: property must not be empty", t.Name)
		}
		if t.Script == "" {
			return nil, fmt.Errorf("trigger %q: script must not be empty", t.Name)
		}
		if !validOperators[t.Operator] {
			return nil, fmt.Errorf("trigger %q: unknown operator %q (want one of ==, !=, >, <)", t.Name, t.Operator)
		}
		if t.DebounceMs <= 0 {
			t.DebounceMs = defaultDebounceMs
		}
		if t.TimeoutSec <= 0 {
			t.TimeoutSec = defaultTimeoutSec
		}
	}
	return triggers, nil
}
