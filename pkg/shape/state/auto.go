// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Host controls for unattended shaping (incident repair and every other
// source that shapes without a person asking): one off switch, one one-run
// override and one append-only decision log, all under <home>/shape.
const (
	autoOffFile      = "auto.off"
	autoOverrideFile = "auto.override"
	autoLogFile      = "auto-decisions.jsonl"
)

// AutoDecision is one line of the decision log.
type AutoDecision struct {
	Time   time.Time `json:"time"`
	Event  string    `json:"event"` // trigger, attempt, verdict, activation, refusal, override, switch
	App    string    `json:"app,omitempty"`
	Source string    `json:"source,omitempty"` // e.g. incident
	Ref    string    `json:"ref,omitempty"`    // the source's item, e.g. an incident id
	Result string    `json:"result"`
	Detail string    `json:"detail,omitempty"`
}

func autoPath(home, name string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errors.New("shape: empty Bashy home")
	}
	return filepath.Join(home, "shape", name), nil
}

// AutoLogPath is the decision log file under home.
func AutoLogPath(home string) (string, error) { return autoPath(home, autoLogFile) }

// AutoEnabled reports whether unattended shaping may start work on this
// host. Only the marker file turns it off; an unresolvable home reads as on,
// which is the behaviour before the switch existed.
func AutoEnabled(home string) bool {
	p, err := autoPath(home, autoOffFile)
	if err != nil {
		return true
	}
	_, err = os.Lstat(p)
	return err != nil
}

// SetAuto turns unattended shaping on or off and logs the change. Turning
// it off also drops an unused override.
func SetAuto(home string, on bool) error {
	p, err := autoPath(home, autoOffFile)
	if err != nil {
		return err
	}
	result := "on"
	if on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		result = "off"
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		if op, err := autoPath(home, autoOverrideFile); err == nil {
			_ = os.Remove(op)
		}
	}
	return LogAuto(home, AutoDecision{Event: "switch", Result: result})
}

// ArmAutoOverride lets the next unattended run exceed the host bound. A
// reason is required and is logged.
func ArmAutoOverride(home, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("shape: --override requires --reason")
	}
	p, err := autoPath(home, autoOverrideFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(reason+"\n"), 0o600); err != nil {
		return err
	}
	return LogAuto(home, AutoDecision{Event: "override", Result: "armed", Detail: reason})
}

// AutoOverridePending returns the reason of an armed, unused override.
func AutoOverridePending(home string) (string, bool) {
	p, err := autoPath(home, autoOverrideFile)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// TakeAutoOverride consumes an armed override: exactly one caller gets it.
func TakeAutoOverride(home string) (string, bool) {
	reason, ok := AutoOverridePending(home)
	if !ok {
		return "", false
	}
	p, _ := autoPath(home, autoOverrideFile)
	if err := os.Remove(p); err != nil {
		return "", false // another run took it
	}
	return reason, true
}

// LogAuto appends one decision to the log. Each call is one write of one
// line on an append-only descriptor, so concurrent writers do not interleave.
func LogAuto(home string, d AutoDecision) error {
	p, err := AutoLogPath(home)
	if err != nil {
		return err
	}
	if d.Time.IsZero() {
		d.Time = time.Now().UTC()
	}
	line, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ReadAutoLog returns the logged decisions, oldest first. A missing log is
// empty; a line that does not parse is skipped.
func ReadAutoLog(home string) ([]AutoDecision, error) {
	p, err := AutoLogPath(home)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AutoDecision
	for _, line := range strings.Split(string(b), "\n") {
		var d AutoDecision
		if strings.TrimSpace(line) == "" || json.Unmarshal([]byte(line), &d) != nil {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}
