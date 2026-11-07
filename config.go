package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"

	"github.com/godbus/dbus/v5"
)

type Trigger struct {
	Name          string   `json:"name"`
	Bus           string   `json:"bus"`
	Sender        string   `json:"sender,omitempty"`
	Path          string   `json:"path,omitempty"`
	Interface     string   `json:"interface"`
	Property      string   `json:"property,omitempty"`
	Signal        string   `json:"signal,omitempty"`
	Argument      int      `json:"argument,omitempty"`
	Operator      string   `json:"operator"`
	ExpectedValue string   `json:"expected_value"`
	Argv          []string `json:"argv"`
	OnlyOnChange  bool     `json:"only_on_change,omitempty"`
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

// D-Bus name grammars. Match rules are built from these values unescaped, and
// the bus only rejects a bad rule at runtime, so check them up front.
var (
	interfaceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	memberName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	busName       = regexp.MustCompile(`^:?[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)+$`)
)

func (t *Trigger) validate() error {
	if t.Bus != "system" && t.Bus != "session" {
		return errors.New("bus must be system or session")
	}
	if !interfaceName.MatchString(t.Interface) {
		return fmt.Errorf("invalid interface %q", t.Interface)
	}
	if (t.Property == "") == (t.Signal == "") {
		return errors.New("exactly one of property or signal is required")
	}
	if t.Signal != "" && !memberName.MatchString(t.Signal) {
		return fmt.Errorf("invalid signal %q", t.Signal)
	}
	if t.Argument < 0 {
		return errors.New("argument must be nonnegative")
	}
	if t.Bus == "system" && t.Sender == "" {
		return errors.New("sender is required on the system bus, where any local user can emit signals")
	}
	if t.Sender != "" && !busName.MatchString(t.Sender) {
		return fmt.Errorf("invalid sender %q", t.Sender)
	}
	if t.Path != "" && !dbus.ObjectPath(t.Path).IsValid() {
		return fmt.Errorf("invalid object path %q", t.Path)
	}
	if !validOperators[t.Operator] {
		return fmt.Errorf("invalid operator %q", t.Operator)
	}
	if t.Operator == ">" || t.Operator == "<" {
		if _, err := strconv.ParseFloat(t.ExpectedValue, 64); err != nil {
			return fmt.Errorf("operator %q needs a numeric expected_value, got %q", t.Operator, t.ExpectedValue)
		}
	}
	if len(t.Argv) == 0 || t.Argv[0] == "" {
		return errors.New("argv must be non-empty")
	}
	if t.DebounceMs < 1 || t.DebounceMs > maxDebounceMs {
		return fmt.Errorf("debounce_ms must be between 1 and %d", maxDebounceMs)
	}
	if t.TimeoutSec < 1 || t.TimeoutSec > maxTimeoutSec {
		return fmt.Errorf("timeout_sec must be between 1 and %d", maxTimeoutSec)
	}
	return nil
}

func loadConfig(path string) ([]Trigger, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var triggers []Trigger
	if err := dec.Decode(&triggers); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("parsing config: trailing data after trigger list")
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
		if t.DebounceMs == 0 {
			t.DebounceMs = defaultDebounceMs
		}
		if t.TimeoutSec == 0 {
			t.TimeoutSec = defaultTimeoutSec
		}
		if err := t.validate(); err != nil {
			return nil, fmt.Errorf("trigger %q: %w", t.Name, err)
		}
	}
	return triggers, nil
}
