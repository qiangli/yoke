package weave

// The capability close-out gate (docs/capability-change-policy.md). A card
// may list the capability IDs its work impacts; `sprint end` then refuses
// until every listed ID has a PASS verdict recorded for the final SHA — the
// HEAD of the checkout `sprint end` runs in (--gate-dir, else cwd) — or an
// explicit, recorded waiver that has not expired. A card listing no IDs is
// unaffected: the gate is opt-in per card, never a new default burden.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type sprintCapability struct {
	ID       string                    `json:"id"`
	Verdicts []sprintCapabilityVerdict `json:"verdicts,omitempty"` // append-only; latest per SHA wins
	Waiver   *sprintCapabilityWaiver   `json:"waiver,omitempty"`
}

type sprintCapabilityVerdict struct {
	SHA    string    `json:"sha"`
	Result string    `json:"result"` // PASS | FAIL
	By     string    `json:"by,omitempty"`
	At     time.Time `json:"at"`
	Note   string    `json:"note,omitempty"`
}

type sprintCapabilityWaiver struct {
	Reason  string    `json:"reason"`
	By      string    `json:"by,omitempty"`
	At      time.Time `json:"at"`
	Expires time.Time `json:"expires"`
}

func findSprintCapability(s *weaveStory, id string) *sprintCapability {
	for i := range s.Capabilities {
		if s.Capabilities[i].ID == id {
			return &s.Capabilities[i]
		}
	}
	return nil
}

// sameCommit matches a recorded SHA against the final one, accepting an
// abbreviated record of at least 7 hex digits.
func sameCommit(recorded, final string) bool {
	recorded, final = strings.ToLower(strings.TrimSpace(recorded)), strings.ToLower(strings.TrimSpace(final))
	if recorded == "" || final == "" {
		return false
	}
	if len(recorded) < 7 {
		return recorded == final
	}
	return strings.HasPrefix(final, recorded)
}

// sprintCapabilityPass reports whether the LATEST verdict recorded for the
// final SHA is PASS: a FAIL after a PASS on the same commit revokes it.
func sprintCapabilityPass(c sprintCapability, finalSHA string) bool {
	pass := false
	for _, v := range c.Verdicts {
		if sameCommit(v.SHA, finalSHA) {
			pass = v.Result == "PASS"
		}
	}
	return pass
}

// sprintCapabilityGateFor is the decision half of the close-out gate, taking
// the final SHA and clock directly so tests need no repository.
func sprintCapabilityGateFor(s *weaveStory, finalSHA string, now time.Time) error {
	var problems []string
	for _, c := range s.Capabilities {
		if sprintCapabilityPass(c, finalSHA) {
			continue
		}
		if w := c.Waiver; w != nil && now.Before(w.Expires) {
			continue
		}
		why := "no PASS verdict for final SHA " + shortSHA(finalSHA)
		if w := c.Waiver; w != nil {
			why += fmt.Sprintf("; waiver by %s expired %s", w.By, w.Expires.UTC().Format(time.RFC3339))
		}
		problems = append(problems, c.ID+": "+why)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("sprint #%d cannot end — %d impacted capability(s) are not proven at the final SHA:\n  %s\n"+
		"  record a verdict:  bashy sprint capability verdict %d <id> --sha <final-sha> --result PASS -m \"<proof>\"\n"+
		"  or, on the record: bashy sprint capability waive %d <id> --reason \"<why>\" --for <duration>",
		s.ID, len(problems), strings.Join(problems, "\n  "), s.ID, s.ID)
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "(unresolved)"
	}
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// sprintCapabilityGate resolves the final SHA in dir (cwd when empty) and
// applies the gate. Cards listing no capability touch no repository.
func sprintCapabilityGate(s *weaveStory, dir string, now time.Time) error {
	if len(s.Capabilities) == 0 {
		return nil
	}
	if strings.TrimSpace(dir) == "" {
		dir, _ = os.Getwd()
	}
	head, _ := gitOut(dir, "rev-parse", "HEAD")
	return sprintCapabilityGateFor(s, strings.TrimSpace(head), now)
}

func newSprintCapabilityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capability",
		Short: "List impacted capability IDs and record their verdicts or waivers for the sprint end gate",
		Long: "A card that lists impacted capability IDs cannot `sprint end` until each ID has a\n" +
			"PASS verdict recorded for the final SHA (HEAD of the checkout `sprint end` runs in),\n" +
			"or an explicit waiver that has not expired. Cards listing no IDs are unaffected.\n" +
			"See docs/capability-change-policy.md.",
	}
	cmd.AddCommand(newSprintCapabilityAddCmd(), newSprintCapabilityVerdictCmd(), newSprintCapabilityWaiveCmd())
	return cmd
}

func newSprintCapabilityAddCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{Use: "add <sprint> <capability-id>", Short: "List a capability ID this sprint impacts", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		capID := strings.TrimSpace(args[1])
		if capID == "" || strings.ContainsAny(capID, " \t\n") {
			return fmt.Errorf("capability id %q must be non-empty with no whitespace", args[1])
		}
		return runWeaveStoryMutate(cmd, id, "sprint capability add", &flags, func(s *weaveStory) (string, error) {
			if findSprintCapability(s, capID) != nil {
				return fmt.Sprintf("sprint #%d already lists capability %s", id, capID), nil
			}
			s.Capabilities = append(s.Capabilities, sprintCapability{ID: capID})
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system", "listed impacted capability "+capID)
			return fmt.Sprintf("sprint #%d lists impacted capability %s", id, capID), nil
		})
	}
	flags.attach(cmd)
	return cmd
}

func newSprintCapabilityVerdictCmd() *cobra.Command {
	var flags weaveOutputFlags
	var sha, result, note string
	cmd := &cobra.Command{Use: "verdict <sprint> <capability-id>", Short: "Record a PASS or FAIL verdict for a capability at a commit", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		result = strings.ToUpper(strings.TrimSpace(result))
		if result != "PASS" && result != "FAIL" {
			return fmt.Errorf("--result must be PASS or FAIL")
		}
		if strings.TrimSpace(sha) == "" {
			cwd, _ := os.Getwd()
			head, err := gitOut(cwd, "rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("--sha not given and HEAD unresolved: %w", err)
			}
			sha = head
		}
		sha = strings.TrimSpace(sha)
		return runWeaveStoryMutate(cmd, id, "sprint capability verdict", &flags, func(s *weaveStory) (string, error) {
			c := findSprintCapability(s, args[1])
			if c == nil {
				return "", fmt.Errorf("sprint #%d lists no capability %q (`sprint capability add %d %s`)", id, args[1], id, args[1])
			}
			who := weaveStoryConductorName(s, "")
			c.Verdicts = append(c.Verdicts, sprintCapabilityVerdict{SHA: sha, Result: result, By: who, At: time.Now().UTC(), Note: strings.TrimSpace(note)})
			weaveStoryAppend(s, who, "review", fmt.Sprintf("capability %s %s at %s: %s", c.ID, result, shortSHA(sha), strings.TrimSpace(note)))
			return fmt.Sprintf("sprint #%d capability %s %s at %s", id, c.ID, result, shortSHA(sha)), nil
		})
	}
	cmd.Flags().StringVar(&sha, "sha", "", "commit the verdict is for (default: HEAD of cwd)")
	cmd.Flags().StringVar(&result, "result", "", "PASS or FAIL")
	cmd.Flags().StringVarP(&note, "message", "m", "", "the proof: check name, run, log reference")
	_ = cmd.MarkFlagRequired("result")
	flags.attach(cmd)
	return cmd
}

func newSprintCapabilityWaiveCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reason string
	var dur time.Duration
	cmd := &cobra.Command{Use: "waive <sprint> <capability-id>", Short: "Record an expiring waiver for a capability's end gate", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return fmt.Errorf("--reason is required: a waiver must say why")
		}
		if dur <= 0 {
			return fmt.Errorf("--for <duration> is required: a waiver must expire")
		}
		return runWeaveStoryMutate(cmd, id, "sprint capability waive", &flags, func(s *weaveStory) (string, error) {
			c := findSprintCapability(s, args[1])
			if c == nil {
				return "", fmt.Errorf("sprint #%d lists no capability %q", id, args[1])
			}
			now := time.Now().UTC()
			who := weaveStoryConductorName(s, "")
			c.Waiver = &sprintCapabilityWaiver{Reason: reason, By: who, At: now, Expires: now.Add(dur)}
			weaveStoryAppend(s, who, "decision", fmt.Sprintf("capability %s waived until %s: %s", c.ID, c.Waiver.Expires.Format(time.RFC3339), reason))
			return fmt.Sprintf("sprint #%d capability %s waived until %s", id, c.ID, c.Waiver.Expires.Format(time.RFC3339)), nil
		})
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why ending without a PASS verdict is right")
	cmd.Flags().DurationVar(&dur, "for", 0, "how long the waiver holds (required)")
	flags.attach(cmd)
	return cmd
}
