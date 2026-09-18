package meet

import (
	"fmt"
	"os"
	"strings"
)

// Action application: turn a meeting's agreed action items into a block a target
// document can carry, so a decision does not die in docs/meetings/.
//
// Default is to PRINT the block. Writing into a document the meeting did not
// author is a mutation the operator should opt into, so `--write` is explicit
// and idempotent (a second apply of the same meeting is a no-op).

// actionsOf collects action items from the explicit human markers plus the
// secretary's synthesis, preserving order and dropping exact duplicates.
func actionsOf(events []Event, syn *Synthesis) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			return
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	for _, e := range events {
		if e.Kind == "action" {
			add(e.Text)
		}
	}
	if syn != nil {
		for _, a := range syn.Actions {
			add(a)
		}
	}
	return out
}

// actionHeading is the idempotence key: one block per meeting id.
func actionHeading(id string) string {
	return fmt.Sprintf("## Action items — meeting `%s`", id)
}

// actionBlock renders the markdown block that `apply` prints or appends.
func actionBlock(st *State, events []Event, syn *Synthesis) string {
	actions := actionsOf(events, syn)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", actionHeading(st.ID))
	fmt.Fprintf(&b, "*%s — %s. Source: `%s`*\n\n",
		st.Topic, st.Created.Format("2006-01-02"), redactHome(minutesPath(st)))
	if len(actions) == 0 {
		b.WriteString("(no action items were agreed)\n")
		return b.String()
	}
	for _, a := range actions {
		fmt.Fprintf(&b, "- [ ] %s\n", redactHome(a))
	}
	return b.String()
}

// applyActions appends the action block to path. It refuses to duplicate a block
// for a meeting already applied there, so re-running after an `amend` is safe
// only when the operator first removes the old block — which is the honest
// behavior: silently rewriting someone's document is worse than refusing.
func applyActions(st *State, events []Event, syn *Synthesis, path string, write bool) (string, error) {
	block := actionBlock(st, events, syn)
	if !write {
		return block, nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("meet: apply --write needs --to <path>")
	}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if strings.Contains(string(existing), actionHeading(st.ID)) {
		return "", fmt.Errorf("meet: %s already carries the action block for %s (remove it first to re-apply)", path, st.ID)
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteString("\n")
	}
	if len(existing) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(block)
	if err := atomicWrite(path, []byte(b.String())); err != nil {
		return "", err
	}
	return block, nil
}
