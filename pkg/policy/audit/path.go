// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package audit

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPath is where bashy's own chain lives: $BASHY_AUDIT when it is a path,
// else $BASHY_HOME/audit/audit.jsonl, else ~/.bashy/audit/audit.jsonl. A boolean
// $BASHY_AUDIT (on/off) selects the default path rather than naming one. ""
// means no home could be found.
func DefaultPath() string {
	v := strings.TrimSpace(os.Getenv("BASHY_AUDIT"))
	switch strings.ToLower(v) {
	case "", "0", "1", "true", "false", "on", "off", "yes", "no":
	default:
		return v
	}
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "audit", "audit.jsonl")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".bashy", "audit", "audit.jsonl")
	}
	return ""
}

// Append writes r to the default chain, filling Time, Host and Actor when the
// caller left them unset. It is the one call for a subsystem that records a
// decision (a forced claim, say) rather than an executed command.
func Append(r Record) (Record, error) {
	path := DefaultPath()
	w, err := Open(path)
	if err != nil {
		return Record{}, err
	}
	if r.Time == "" {
		r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if r.Host == "" {
		r.Host, _ = os.Hostname()
	}
	if r.Actor == (Actor{}) {
		r.Actor = ActorFromEnv()
	}
	return w.Append(r)
}
