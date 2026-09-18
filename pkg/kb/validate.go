package kb

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

func newValidateCmd(dir, ring *string) *cobra.Command {
	var evidence, fromGate string
	cmd := &cobra.Command{
		Use:   "validate <slug>",
		Short: "Promote a candidate to validated",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if strings.TrimSpace(evidence) == "" && strings.TrimSpace(fromGate) == "" {
				return fmt.Errorf("kb: validate requires --evidence or --from-gate")
			}
			store := openRing(*dir, *ring)
			if fromGate != "" {
				ev, err := findObserveEvent(store, strings.TrimSpace(fromGate))
				if err != nil {
					return err
				}
				if err := validateGateEvent(ev); err != nil {
					return err
				}
				if strings.TrimSpace(evidence) == "" {
					evidence = "gate:" + ev.ID
				}
			}
			p, err := store.Load(args[0])
			if err != nil {
				return err
			}
			p.Status = StatusValidated
			p.Evidence = strings.TrimSpace(evidence)
			if err := store.Write(p, "validate"); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "validated %s\n", p.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&evidence, "evidence", "", "how it was verified (command, commit, issue)")
	cmd.Flags().StringVar(&fromGate, "from-gate", "", "observe event id for a gate that ran and passed")
	return cmd
}

func findObserveEvent(store *Store, id string) (observeEvent, error) {
	f, err := os.Open(store.journalPath())
	if err != nil {
		if os.IsNotExist(err) {
			return observeEvent{}, fmt.Errorf("kb: no such observe event %q", id)
		}
		return observeEvent{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev observeEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.ID == id {
			return ev, nil
		}
	}
	if err := sc.Err(); err != nil {
		return observeEvent{}, err
	}
	return observeEvent{}, fmt.Errorf("kb: no such observe event %q", id)
}

func validateGateEvent(ev observeEvent) error {
	if ev.Kind != "gate" {
		return fmt.Errorf("kb: observe event %q is not a gate (kind=%s)", ev.ID, ev.Kind)
	}
	if !ev.Ran {
		return fmt.Errorf("kb: gate observe event %q did not run", ev.ID)
	}
	if !ev.Passed {
		return fmt.Errorf("kb: gate observe event %q did not pass", ev.ID)
	}
	return nil
}
