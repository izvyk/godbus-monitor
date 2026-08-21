package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/godbus/dbus/v5"
)

// Trigger defines a single D-Bus event rule
type Trigger struct {
	Bus           string `json:"bus"`            // "system" or "session"
	Interface     string `json:"interface"`      // e.g., "org.gnome.Mutter.DisplayConfig"
	Property      string `json:"property"`       // e.g., "PowerSaveMode"
	Operator      string `json:"operator"`       // "==", "!=", ">", "<"
	ExpectedValue string `json:"expected_value"` // Stringified value, e.g., "0", "true"
	Script        string `json:"script"`         // Bash commands to run
}

type wrappedSignal struct {
	Bus    string
	Signal *dbus.Signal
}

// evaluateCondition handles type-agnostic comparisons
func evaluateCondition(actual interface{}, operator, expectedStr string) bool {
	actualStr := fmt.Sprintf("%v", actual)

	if operator == "==" || operator == "" {
		return actualStr == expectedStr
	}
	if operator == "!=" {
		return actualStr != expectedStr
	}

	// For greater/less than, try parsing both as floats
	actualFloat, err1 := strconv.ParseFloat(actualStr, 64)
	expectedFloat, err2 := strconv.ParseFloat(expectedStr, 64)
	if err1 == nil && err2 == nil {
		if operator == ">" {
			return actualFloat > expectedFloat
		}
		if operator == "<" {
			return actualFloat < expectedFloat
		}
	}
	return false
}

func runScript(command string) {
	if command == "" {
		return
	}
	cmd := exec.Command("sh", "-c", command)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Error executing script: %v\n", err)
	}
}

func main() {
	configPath := flag.String("config", "triggers.json", "Path to JSON config")
	flag.Parse()

	// 1. Load Configuration
	file, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Printf("Failed to read config: %v\n", err)
		os.Exit(1)
	}

	var triggers []Trigger
	if err := json.Unmarshal(file, &triggers); err != nil {
		fmt.Printf("Failed to parse config: %v\n", err)
		os.Exit(1)
	}

	// 2. Determine which buses we actually need
	needsSystem, needsSession := false, false
	for _, t := range triggers {
		if t.Bus == "system" {
			needsSystem = true
		} else if t.Bus == "session" {
			needsSession = true
		}
	}

	var sysConn, sessConn *dbus.Conn
	events := make(chan wrappedSignal, 20)

	// 3. Connect and Subscribe to System Bus
	if needsSystem {
		sysConn, err = dbus.SystemBus()
		if err != nil {
			fmt.Printf("System Bus error: %v\n", err)
			os.Exit(1)
		}
		defer sysConn.Close()

		sysChan := make(chan *dbus.Signal, 10)
		sysConn.Signal(sysChan)
		go func() {
			for sig := range sysChan {
				events <- wrappedSignal{Bus: "system", Signal: sig}
			}
		}()
	}

	// 4. Connect and Subscribe to Session Bus
	if needsSession {
		sessConn, err = dbus.SessionBus()
		if err != nil {
			fmt.Printf("Session Bus error: %v\n", err)
			os.Exit(1)
		}
		defer sessConn.Close()

		sessChan := make(chan *dbus.Signal, 10)
		sessConn.Signal(sessChan)
		go func() {
			for sig := range sessChan {
				events <- wrappedSignal{Bus: "session", Signal: sig}
			}
		}()
	}

	// 5. Register Match Signals dynamically
	for _, t := range triggers {
		conn := sessConn
		if t.Bus == "system" {
			conn = sysConn
		}
		
		conn.AddMatchSignal(
			dbus.WithMatchInterface("org.freedesktop.DBus.Properties"),
			dbus.WithMatchMember("PropertiesChanged"),
			dbus.WithMatchArg(0, t.Interface),
		)
	}

	fmt.Println("⚡ Generic D-Bus Trigger running...")

	// 6. The Unified Event Loop
	for e := range events {
		if len(e.Signal.Body) < 2 {
			continue
		}

		iface, ok := e.Signal.Body[0].(string)
		if !ok {
			continue
		}

		props, ok := e.Signal.Body[1].(map[string]dbus.Variant)
		if !ok {
			continue
		}

		// Cross-reference received properties against our JSON rules
		for _, t := range triggers {
			if t.Bus == e.Bus && t.Interface == iface {
				if variant, exists := props[t.Property]; exists {
					
					if evaluateCondition(variant.Value(), t.Operator, t.ExpectedValue) {
						fmt.Printf("Match: [%s] %s %s %s\n", t.Interface, t.Property, t.Operator, t.ExpectedValue)
						go runScript(t.Script)
					}
					
				}
			}
		}
	}
}
