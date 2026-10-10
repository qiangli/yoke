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

// targetArgs is how the claim is addressed on the `bashy claim` command line,
// as the arguments that follow the verb. A printed command is only useful if it
// reaches THIS claim when pasted anywhere, which rules out two shortcuts:
//
//   - "kind:name" is wrong whenever ParseRef would not read it back. A kind of
//     one letter is indistinguishable from a Windows drive letter, so "x:foo"
//     parses as the plain name "x:foo" — a different target, which `claim
//     --wait` would happily acquire while the real holder keeps refusing. Those
//     render as the explicit `--kind K NAME` form instead.
//   - a project claim cannot render as nothing. The no-argument forms act on the
//     CALLER's project, so a paste from another directory would address a
//     different project — request nobody, or wait on the caller's own claim. It
//     addresses the conflicting PATH SET instead, which is what made this a
//     conflict in the first place and reads the same from anywhere.
func (c *Conflict) targetArgs() []string {
	if c.Claim.Resource == "" {
		if root := c.projectRoot(); root != "" {
			return []string{"--kind", "path", root}
		}
		return nil
	}
	return refArgs(c.Claim.Address())
}

// refArgs addresses one ref on the `bashy claim` command line: the plain
// "kind:name" word when ParseRef reads it back as the same ref, the explicit
// `--kind K NAME` pair when it would not.
func refArgs(r Ref) []string {
	if ParseRef(r.String()) == r {
		return []string{r.String()}
	}
	return []string{"--kind", r.Kind, r.Name}
}

// shellWords renders command arguments as quoted shell words, each preceded by
// a space, ready to append to a printed command.
func shellWords(args []string) string {
	var b strings.Builder
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// projectRoot is one path out of the conflicting project's path set. Any member
// addresses the whole claim, since project claims conflict by intersection.
func (c *Conflict) projectRoot() string {
	c.Claim.normalize()
	for _, p := range append(append([]string(nil), c.Claim.Roots...), c.Claim.Members...) {
		if strings.TrimSpace(p) != "" {
			return p
		}
	}
	return ""
}

// target is targetArgs as shell words, ready to append to a printed command.
func (c *Conflict) target() string { return shellWords(c.targetArgs()) }

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
		fmt.Fprintf(&b, "  bashy claim release%s          # the holder releases when finished\n", c.target())
		return b.String()
	}
	fmt.Fprintf(&b, "  bashy weave add \"<task>\"        # work in an ISOLATED workspace instead\n")
	fmt.Fprintf(&b, "  BASHY_CLAIM_FORCE=1 <command>   # override (recorded in the audit log)\n")
	return b.String()
}
