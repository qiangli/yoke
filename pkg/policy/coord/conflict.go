// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package coord

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ConflictSchema is the machine-readable form of a refusal.
const ConflictSchema = "bashy-claim-conflict-v1"

// Conflict is a live claim held by someone else over what we want.
type Conflict struct {
	Claim *Claim
	// now pins the liveness verdict; zero means the present.
	now time.Time
}

func (c *Conflict) at() time.Time {
	if c.now.IsZero() {
		return time.Now()
	}
	return c.now
}

// holder is the name to address the other party by.
func (c *Conflict) holder() string {
	h := c.Claim.Holder
	switch {
	case h.Name != "":
		return h.Name
	case h.Episode != "":
		return h.Episode
	case h.Kind != "":
		return string(h.Kind)
	}
	return ""
}

// target is how the claim is addressed on the `bashy claim` command line, as
// extra arguments: nothing for a project claim (the no-argument forms act on
// the caller's own project), the quoted kind:name ref for a resource.
func (c *Conflict) target() string {
	if c.Claim.Resource == "" {
		return ""
	}
	return " " + shellQuote(c.Claim.Address().String())
}

// shellQuote leaves a plain word as it is and single-quotes anything a shell
// would split or expand, so a printed command parses back to the same argument.
func shellQuote(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Contacts are copy-paste commands for reaching the holder or waiting them out.
// Every `bashy claim` line parses with NewClaimCmd.
func (c *Conflict) Contacts() []string {
	who := c.holder()
	if who == "" {
		who = "<holder>"
	}
	t := c.target()
	return []string{
		fmt.Sprintf(`bashy ping %s "<why you need it>"`, who),
		fmt.Sprintf(`bashy claim request%s -m "<reason>"`, t),
		fmt.Sprintf("bashy meet dm %s", who),
		fmt.Sprintf("bashy claim%s --wait 30m", t),
	}
}

type conflictJSON struct {
	Schema   string   `json:"schema_version"`
	Holder   string   `json:"holder"`
	Resource string   `json:"resource"`
	Kind     string   `json:"kind"`
	Intent   string   `json:"intent"`
	Since    string   `json:"since"`
	Liveness string   `json:"liveness"`
	Contacts []string `json:"contacts"`
}

// JSON renders the refusal as a bashy-claim-conflict-v1 document.
func (c *Conflict) JSON() ([]byte, error) {
	ref := c.Claim.Address()
	return json.MarshalIndent(conflictJSON{
		Schema:   ConflictSchema,
		Holder:   c.holder(),
		Resource: ref.String(),
		Kind:     ref.Kind,
		Intent:   c.Claim.Intent,
		Since:    c.Claim.AcquiredAt.UTC().Format(time.RFC3339),
		Liveness: string(c.Claim.Liveness(c.at())),
		Contacts: c.Contacts(),
	}, "", "  ")
}

func (c *Conflict) Error() string {
	who := c.holder()
	if who == "" {
		who = "another agent"
	}
	ref := c.Claim.Address().String()
	var b strings.Builder
	fmt.Fprintf(&b, "%s already holds %s", who, ref)
	host := c.Claim.Holder.Host
	if host == "" {
		host = "this host"
	}
	fmt.Fprintf(&b, " on %s", host)
	if c.Claim.Intent != "" {
		fmt.Fprintf(&b, " (%s)", c.Claim.Intent)
	}
	fmt.Fprintf(&b, ", since %s, %s.\n\n", c.Claim.AcquiredAt.Format(time.Kitchen), c.Claim.Liveness(c.at()))
	if c.Claim.Resource != "" {
		fmt.Fprintf(&b, "This advisory hold coordinates agents on this host; it does not prove the remote resource is idle.\n\n")
	} else {
		fmt.Fprintf(&b, "Two agents writing one project is how an untested change reaches main: one session\n")
		fmt.Fprintf(&b, "sweeps another's staged work into its commit, and nobody can tell whose edit was whose.\n\n")
	}
	fmt.Fprintf(&b, "Reach the holder, or wait:\n")
	for _, line := range c.Contacts() {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	fmt.Fprintf(&b, "\n  bashy claim list                # who is working, where, on what\n")
	if c.Claim.Resource != "" {
		fmt.Fprintf(&b, "  bashy claim release %s          # the holder releases when finished\n", shellQuote(ref))
		return b.String()
	}
	fmt.Fprintf(&b, "  bashy weave add \"<task>\"        # work in an ISOLATED workspace instead\n")
	fmt.Fprintf(&b, "  BASHY_CLAIM_FORCE=1 <command>   # override (recorded in the audit log)\n")
	return b.String()
}
