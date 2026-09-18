package herald

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// NewHeraldCmd builds the `bashy herald` verb tree.
//
// The grammar follows the official `a2a` CLI so muscle memory transfers:
// verb, then target, then operands. What herald adds over that CLI is the
// three things it does not have — a persistent address book, credentials from
// the vault rather than a raw header flag, and a gate on the result.
func NewHeraldCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "herald",
		Short: "Reach agents that are not on this host (A2A)",
		Long: strings.TrimSpace(`
herald reaches agents that are not on this host.

Every other coordination verb — meet, weave, foreman, delegate — addresses a
participant as a tool:model binding resolving to a binary HERE. herald is the
path to a capability that is not installed here and cannot be: an agent on
someone else's infrastructure, over the Agent2Agent protocol.

A peer is an ordinary binding. ` + "`herald add reviewer https://…`" + ` writes a
fleet model, so ` + "`herald:reviewer`" + ` is then addressable by meet, weave and
foreman with no further setup.

A peer's own "completed" is accepted as an unverified result. Pass --gate when
you need an authoritative verdict; herald runs it and reports the result.`),
		SilenceUsage: true,
	}
	root.AddCommand(newDiscoverCmd(), newAddCmd(), newListCmd(), newRemoveCmd(), newSendCmd(), newACPCmd())
	return root
}

func bookFor(cmd *cobra.Command) *Book {
	root, _ := cmd.Flags().GetString("fleet-root")
	return NewBook(root)
}

func addCommonFlags(c *cobra.Command) {
	c.Flags().String("fleet-root", "", "fleet root holding the address book (default: the bashy fleet dir)")
	c.Flags().Bool("json", false, "emit a machine-readable envelope")
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newDiscoverCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "discover <url|peer>",
		Short: "Fetch and show a peer's agent card",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
				p, err := bookFor(cmd).Get(target)
				if err != nil {
					return err
				}
				target = p.URL
			}
			card, err := Discover(cmd.Context(), target)
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return emitJSON(struct {
					SchemaVersion string `json:"schema_version"`
					Card          Card   `json:"card"`
				}{SchemaVersion, card})
			}
			fmt.Printf("%s", card.Name)
			if card.Version != "" {
				fmt.Printf("  v%s", card.Version)
			}
			fmt.Println()
			if card.Description != "" {
				fmt.Printf("  %s\n", card.Description)
			}
			fmt.Printf("  streaming: %v\n", card.Streaming)
			if card.SupportsGate() {
				fmt.Printf("  gate extension: yes\n")
			} else {
				// Not a warning — it is the normal case, and herald handles
				// it by running the gate itself.
				fmt.Printf("  gate extension: no (herald will gate locally)\n")
			}
			for _, s := range card.Skills {
				fmt.Printf("  skill %s", s.ID)
				if s.Name != "" {
					fmt.Printf(" — %s", s.Name)
				}
				fmt.Println()
			}
			if len(card.Skills) > 0 {
				fmt.Println("\n  (skills are the peer's own claims; a band is earned from a gate, not read from a card)")
			}
			return nil
		},
	}
	addCommonFlags(c)
	return c
}

func newAddCmd() *cobra.Command {
	var keyRef, display string
	c := &cobra.Command{
		Use:   "add <name> <url>",
		Short: "Add a peer to the address book",
		Long: strings.TrimSpace(`
Adds a peer, making it addressable as herald:<name> — an ordinary tool:model
binding that meet, weave and foreman resolve with no special case.

The peer is recorded UNPEGGED (band 0). A card's skills[] are self-asserted;
a band is earned from a gate this host ran.`),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := Peer{Name: args[0], URL: args[1], APIKeyRef: keyRef, Display: display}
			b := bookFor(cmd)
			if err := b.Add(p); err != nil {
				return err
			}
			// Reachability is a fact worth checking now rather than at the
			// first delegation, but an unreachable peer is still recorded:
			// a laptop peer that is merely asleep should not be un-addable.
			if _, err := Discover(cmd.Context(), p.URL); err != nil {
				fmt.Fprintf(os.Stderr, "herald: added %s, but its card is not reachable yet: %v\n", p.Name, err)
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return emitJSON(struct {
					SchemaVersion string `json:"schema_version"`
					Peer          Peer   `json:"peer"`
				}{SchemaVersion, p})
			}
			fmt.Printf("added %s → %s (addressable as %s)\n", p.Name, p.URL, p.Binding())
			return nil
		},
	}
	c.Flags().StringVar(&keyRef, "api-key-ref", "", "name of the vault secret holding this peer's credential (never the value)")
	c.Flags().StringVar(&display, "display", "", "human-readable label")
	addCommonFlags(c)
	return c
}

func newListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List peers in the address book",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			peers, err := bookFor(cmd).List()
			if err != nil {
				return err
			}
			if j, _ := cmd.Flags().GetBool("json"); j {
				return emitJSON(struct {
					SchemaVersion string `json:"schema_version"`
					Peers         []Peer `json:"peers"`
				}{SchemaVersion, peers})
			}
			if len(peers) == 0 {
				fmt.Println("no peers — add one with `bashy herald add <name> <url>`")
				return nil
			}
			fmt.Printf("%-20s %-8s %s\n", "NAME", "BAND", "URL")
			for _, p := range peers {
				band := fmt.Sprintf("%d", p.Band)
				if p.Band == 0 {
					band = "-"
				}
				fmt.Printf("%-20s %-8s %s\n", p.Name, band, p.URL)
			}
			return nil
		},
	}
	addCommonFlags(c)
	return c
}

func newRemoveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove"},
		Short:   "Remove a peer from the address book",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := bookFor(cmd).Remove(args[0]); err != nil {
				return err
			}
			fmt.Printf("removed %s\n", args[0])
			return nil
		},
	}
	addCommonFlags(c)
	return c
}

func newSendCmd() *cobra.Command {
	var gate, gateDir string
	var stream bool
	c := &cobra.Command{
		Use:   "send <peer> <prompt>",
		Short: "Delegate a task to a peer, optionally gating the result",
		Long: strings.TrimSpace(`
Delegates a task and returns the peer's result.

With --gate, that command is authoritative: exit 0 only when it passes. So a
gated peer composes:

    bashy herald send reviewer "review PR 41" --gate './ci.sh' && echo shipped

Without --gate, completed peer work exits 0 and is clearly reported as
UNVERIFIED.`),
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := bookFor(cmd).Get(args[0])
			if err != nil {
				return err
			}
			prompt := strings.Join(args[1:], " ")
			jsonOut, _ := cmd.Flags().GetBool("json")

			opts := SendOptions{Gate: gate, GateDir: gateDir, Stream: stream}
			if stream && !jsonOut {
				// Line-buffered to stdout so the peer is pipeable WHILE it
				// runs, like any other command in a shell pipeline.
				opts.OnText = func(s string) { fmt.Print(s) }
			}

			res, err := Send(cmd.Context(), p, prompt, opts)
			if err != nil {
				return err
			}
			if jsonOut {
				if err := emitJSON(res); err != nil {
					return err
				}
			} else {
				if opts.OnText == nil && res.Text != "" {
					fmt.Println(strings.TrimRight(res.Text, "\n"))
				}
				// Diagnostics on stderr; stdout carries the RESULT only, so
				// the peer behaves like a well-formed command in a pipeline.
				fmt.Fprintf(os.Stderr, "herald: %s reported %s; gate %s\n", p.Name, res.State, res.Gate.Summary())
			}
			if code := res.ExitCode(); code != 0 {
				return &exitError{code: code}
			}
			return nil
		},
	}
	c.Flags().StringVar(&gate, "gate", "", "command that decides whether the peer's work is good (exit 0 = pass)")
	c.Flags().StringVar(&gateDir, "gate-dir", "", "directory the gate runs in (default: cwd)")
	c.Flags().BoolVar(&stream, "stream", false, "stream incremental output as it arrives")
	addCommonFlags(c)
	return c
}

// exitError carries a specific exit status out of RunE without printing a
// second diagnostic — the caller already explained itself.
type exitError struct{ code int }

func (e *exitError) Error() string { return "" }

// ExitCode lets a host map the error onto os.Exit.
func (e *exitError) ExitCode() int { return e.code }

// Run executes the herald tree with args and returns a process exit status.
// Hosts that dispatch verbs (bashy's agentos) use this so the gate verdict
// reaches the shell.
func Run(ctx context.Context, args []string) int {
	cmd := NewHeraldCmd()
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		var ee *exitError
		if asExit(err, &ee) {
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "bashy herald:", err)
		return 1
	}
	return 0
}

func asExit(err error, target **exitError) bool {
	for err != nil {
		if e, ok := err.(*exitError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
