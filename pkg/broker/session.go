package broker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sessions follow shell scope (Sprint 255 §3; Q5). A session handle is a
// string whose shape IS its lineage, so a shell can derive a clone or a child
// handle without asking the broker:
//
//	s-<hex>                 a root session (a new shell)
//	<parent>.c<hex>@<ms>    a clone (subshell, parallel attempt): sees the
//	                        parent's items created up to <ms>; its own
//	                        items never flow back
//	<parent>~               the EXPORTED VIEW of <parent>: what a shell puts
//	                        in the environment for its children
//	<parent>~<hex>          a child shell minted from that view: sees only
//	                        what its ancestors exported
//
// Gate executors start empty because the attest env allowlist drops the
// handle; a request with no handle belongs to the principal scope and sees
// only principal-scope items.

// SessionEnv is the environment variable that carries the exported view.
const SessionEnv = "BASHY_MODEL_SESSION"

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// NewRootSession mints a root session handle.
func NewRootSession() string { return "s-" + randHex(6) }

// CloneSession derives a clone of parent at time now.
func CloneSession(parent string, now time.Time) string {
	return fmt.Sprintf("%s.c%s@%d", parent, randHex(4), now.UnixMilli())
}

// ExportedView is the handle a shell exports for its children.
func ExportedView(self string) string {
	if self == "" || strings.HasSuffix(self, "~") {
		return self
	}
	return self + "~"
}

// ShellSession resolves the session a starting shell owns, from the handle
// it inherited (inherited may be ""): a child of that exported view, or a new
// root. The shell exports ExportedView(result) to its own children.
func ShellSession(inherited string) string {
	inherited = strings.TrimSpace(inherited)
	if inherited == "" || ValidateSession(inherited) != nil {
		return NewRootSession()
	}
	return ExportedView(inherited) + randHex(4)
}

// ShellSessionFromEnv applies ShellSession to $BASHY_MODEL_SESSION and
// exports the new session's view, returning the shell's own handle.
func ShellSessionFromEnv() string {
	self := ShellSession(os.Getenv(SessionEnv))
	_ = os.Setenv(SessionEnv, ExportedView(self))
	return self
}

// ValidateSession checks a handle's grammar.
func ValidateSession(h string) error {
	if h == "" {
		return nil
	}
	if len(h) > 1024 {
		return fmt.Errorf("session handle too long")
	}
	if !strings.HasPrefix(h, "s-") {
		return fmt.Errorf("session handle %q must start with s-", h)
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '~', r == '@':
		default:
			return fmt.Errorf("session handle %q has an invalid character %q", h, r)
		}
	}
	return nil
}

// lineageStep is one ancestor of a session and the rule for seeing its items.
type lineageStep struct {
	id           string
	exportedOnly bool      // crossed a child boundary on the way up
	cutoff       time.Time // zero = no time cutoff
}

// lineage lists the session itself first, then each ancestor with the
// visibility rule that applies to its items.
func lineage(h string) []lineageStep {
	if h == "" {
		return nil
	}
	steps := []lineageStep{{id: h}}
	exportedOnly := false
	var cutoff time.Time
	cur := h
	for {
		parent, kind, t := parentOf(cur)
		if parent == "" {
			break
		}
		switch kind {
		case '~':
			exportedOnly = true
		case '.':
			if !t.IsZero() && (cutoff.IsZero() || t.Before(cutoff)) {
				cutoff = t
			}
		}
		steps = append(steps, lineageStep{id: parent, exportedOnly: exportedOnly, cutoff: cutoff})
		cur = parent
	}
	return steps
}

// parentOf splits off the last lineage segment. kind is '.' for a clone and
// '~' for a child/exported view; t is a clone's fork time.
func parentOf(h string) (parent string, kind byte, t time.Time) {
	i := strings.LastIndexAny(h, ".~")
	if i <= 0 {
		return "", 0, time.Time{}
	}
	kind = h[i]
	parent = h[:i]
	if kind == '.' {
		seg := h[i+1:]
		if at := strings.LastIndexByte(seg, '@'); at >= 0 {
			if ms, err := strconv.ParseInt(seg[at+1:], 10, 64); err == nil {
				t = time.UnixMilli(ms)
			}
		}
	}
	return parent, kind, t
}

// scoped is anything stored in a session scope.
type scoped struct {
	Principal string    `json:"principal"`
	Session   string    `json:"session,omitempty"`
	Exported  bool      `json:"exported,omitempty"`
	Created   time.Time `json:"created"`
}

// visible reports whether an item stored in scope item is visible to a
// request from principal in session h.
func visible(item scoped, principal, h string) bool {
	if item.Principal != principal {
		return false
	}
	if item.Session == "" {
		return true // principal scope: visible to every session of the principal
	}
	for _, st := range lineage(h) {
		if st.id != item.Session {
			continue
		}
		if st.exportedOnly && !item.Exported {
			return false
		}
		if !st.cutoff.IsZero() && item.Created.After(st.cutoff) {
			return false
		}
		return true
	}
	return false
}
