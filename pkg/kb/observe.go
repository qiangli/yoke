package kb

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

type observeEvent struct {
	ID       string `json:"id"`
	Event    string `json:"event"`
	Kind     string `json:"kind"`
	Episode  string `json:"episode,omitempty"`
	Ref      string `json:"ref"`
	Summary  string `json:"summary,omitempty"`
	At       string `json:"at,omitempty"`
	Ran      bool   `json:"ran,omitempty"`
	Passed   bool   `json:"passed,omitempty"`
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Where    string `json:"where,omitempty"`
	Tool     string `json:"tool,omitempty"`
}

func newObserveCmd(dir, ring *string) *cobra.Command {
	var (
		episode, kind, ref, summary, at string
		jsonOut                         bool
		ran, passed                     bool
		command, where                  string
		exitCode                        int
	)
	cmd := &cobra.Command{
		Use:   "observe",
		Short: "Append a kb observation event",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			kind = strings.TrimSpace(kind)
			episode = strings.TrimSpace(episode)
			ref = strings.TrimSpace(ref)
			at = strings.TrimSpace(at)
			if episode == "" {
				return fmt.Errorf("kb: observe requires --episode")
			}
			if kind == "" {
				return fmt.Errorf("kb: observe requires --kind")
			}
			if ref == "" {
				return fmt.Errorf("kb: observe requires --ref")
			}
			ev := observeEvent{
				ID:       observeEventID(kind, episode, ref, at),
				Event:    "observe",
				Kind:     kind,
				Episode:  episode,
				Ref:      ref,
				Summary:  strings.TrimSpace(summary),
				At:       at,
				Tool:     ToolID(),
				Ran:      ran,
				Passed:   passed,
				Command:  strings.TrimSpace(command),
				ExitCode: exitCode,
				Where:    strings.TrimSpace(where),
			}
			if err := appendObserveEvent(openRing(*dir, *ring), ev); err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(ev)
			}
			fmt.Fprintln(c.OutOrStdout(), ev.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&episode, "episode", "", "episode id")
	cmd.Flags().StringVar(&kind, "kind", "", "observation kind: tool-result|gate|note|...")
	cmd.Flags().StringVar(&ref, "ref", "", "payload reference")
	cmd.Flags().StringVar(&summary, "summary", "", "short observation summary")
	cmd.Flags().StringVar(&at, "at", "", "event timestamp or logical time used in the deterministic id")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the appended event as JSON")
	cmd.Flags().BoolVar(&ran, "ran", false, "gate outcome: whether the gate ran")
	cmd.Flags().BoolVar(&passed, "passed", false, "gate outcome: whether the gate passed")
	cmd.Flags().StringVar(&command, "command", "", "gate command")
	cmd.Flags().IntVar(&exitCode, "exit-code", 0, "gate exit code")
	cmd.Flags().StringVar(&where, "where", "", "where the gate ran")
	return cmd
}

func observeEventID(kind, episode, ref, at string) string {
	h := sha1.New()
	for i, part := range []string{kind, episode, ref, at} {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func appendObserveEvent(store *Store, ev observeEvent) error {
	if err := os.MkdirAll(store.Dir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(store.journalPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
