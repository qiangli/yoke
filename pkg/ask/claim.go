package ask

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ctty"
)

// newClaimCmd is the verb a HUMAN runs, in their own terminal, to answer a
// request raised somewhere else.
//
// This is the rung that always works — no GUI, no shared terminal, nothing to
// install — and it is the safe replacement for the /tmp/x habit. The value is
// typed here, in a terminal the requesting program has no access to, and travels
// to it over a private channel.
func newClaimCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "claim [ID]",
		Short: "answer a pending request, interactively, from your own terminal",
		Long: `claim shows a pending request — who raised it, from where, and where the
answer will go — and reads your value with the input hidden.

Run it in a terminal YOU control. With no ID it picks the single pending
request, or lists them when there is more than one. A unique id prefix is
enough.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := pick(args)
			if err != nil {
				return err
			}
			return claim(c, r)
		},
	}
	return cmd
}

// pick resolves the request to answer, refusing to guess when it is ambiguous.
func pick(args []string) (Request, error) {
	if len(args) == 1 {
		return Find(args[0])
	}
	all, err := List()
	if err != nil {
		return Request{}, err
	}
	pending := make([]Request, 0, len(all))
	now := time.Now()
	for _, r := range all {
		if r.Pending(now) && !hasValue(r.ID) {
			pending = append(pending, r)
		}
	}
	switch len(pending) {
	case 1:
		return pending[0], nil
	case 0:
		return Request{}, fmt.Errorf("ask: nothing is waiting for an answer")
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "ask: %d requests are waiting — name one:\n", len(pending))
		for _, r := range pending {
			fmt.Fprintf(&b, "  bashy ask claim %s   %s\n", shortID(r.ID), r.summary())
		}
		return Request{}, errors.New(strings.TrimRight(b.String(), "\n"))
	}
}

// claim renders the provenance frame and reads the value.
//
// The frame is rendered HERE, by the answering side, and that placement is
// deliberate: it is built from what bashy recorded about the requester when the
// request was raised, so the requesting program cannot influence how its own
// provenance is displayed. Only the prompt is its text, and it arrives sanitized
// and clearly labelled as untrusted.
func claim(c *cobra.Command, r Request) error {
	if hasValue(r.ID) {
		return fmt.Errorf("ask: request %s has already been answered", shortID(r.ID))
	}
	if !r.Pending(time.Now()) {
		return fmt.Errorf("ask: request %s has expired", shortID(r.ID))
	}

	// Prefer this terminal. `claim` is interactive by definition — a human just
	// typed it — so unlike the requesting side there is no ladder to walk.
	value, err := readHere(c, r)
	if err != nil {
		return err
	}
	if err := Answer(r, value); err != nil {
		return err
	}
	fmt.Fprintf(c.ErrOrStderr(), "delivered to %s\n", requesterLabel(r.Requester))
	return nil
}

// readHere reads the value on the operator's own terminal, falling back to this
// command's stdin when there is no terminal (a piped `claim`, which is unusual but
// legitimate in a script).
func readHere(c *cobra.Command, r Request) ([]byte, error) {
	frame := renderFrame(r)
	prompt := promptLine(r)

	if t, err := ctty.Open(); err == nil {
		defer t.Close()
		fmt.Fprintln(t, frame)
		if r.Secret {
			return t.ReadSecret(prompt)
		}
		return t.ReadLine(prompt)
	}

	fmt.Fprintln(c.ErrOrStderr(), frame)
	fmt.Fprint(c.ErrOrStderr(), prompt)
	b, err := io.ReadAll(c.InOrStdin())
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(c.ErrOrStderr())
	return trimTerminator(b), nil
}

// newAnswerCmd is the verb a HARNESS runs: same delivery, value on stdin, no
// prompting. It is what a first-party UI calls after collecting the value in its
// own modal, and it is also the scriptable form of claim.
func newAnswerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "answer ID",
		Short: "deliver a value to a pending request, reading it from stdin",
		Long: `answer delivers a value to a pending request without prompting. The value is
read from stdin, so it never appears in the process table or in shell history:

  printf %s "$value" | bashy ask answer a7f3c1d2

This is the callback a first-party harness uses after collecting the value in
its own UI (see the BASHY-ASK-V1 line ask prints on stderr).`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := Find(args[0])
			if err != nil {
				return err
			}
			b, err := io.ReadAll(c.InOrStdin())
			if err != nil {
				return err
			}
			value := trimTerminator(b)
			if len(value) == 0 {
				// Same rule as every other channel: an empty answer is a decline,
				// never a successful empty secret.
				return fmt.Errorf("ask: refusing to deliver an empty value")
			}
			if err := Answer(r, value); err != nil {
				return err
			}
			fmt.Fprintf(c.ErrOrStderr(), "delivered to %s\n", requesterLabel(r.Requester))
			return nil
		},
	}
	return cmd
}

// trimTerminator removes ONE trailing newline — the one the shell or the terminal
// added — and nothing else. Trailing spaces and tabs are preserved because some
// tokens legitimately contain them, and a silently mangled credential produces an
// authentication failure nobody can explain.
func trimTerminator(b []byte) []byte {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return []byte(s)
}

func newLsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "show pending requests and delivered values",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			// Listing reaps: reading IS the reconciliation, so the board never
			// asserts that an expired request is live.
			all, err := List()
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			if asJSON {
				return json.NewEncoder(w).Encode(map[string]any{
					"schema_version": SchemaVersion,
					"requests":       all,
				})
			}
			if len(all) == 0 {
				fmt.Fprintln(w, "nothing pending")
				return nil
			}
			fmt.Fprintf(w, "%-10s %-16s %-9s %s\n", "ID", "NAME", "STATE", "REQUESTED BY")
			for _, r := range all {
				fmt.Fprintf(w, "%-10s %-16s %-9s %s\n",
					shortID(r.ID), orDash(r.Name), r.state(time.Now()), requesterLabel(r.Requester))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel ID",
		Short: "withdraw a pending request and remove any value delivered under it",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := Find(args[0])
			if err != nil {
				return err
			}
			if err := Cancel(r.ID); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "cancelled %s\n", shortID(r.ID))
			return nil
		},
	}
}

// --- shared output helpers -----------------------------------------------

// report prints the result of a successful ask.
//
// For the file sinks this is a PATH; only --stdout puts the value here. That
// distinction is the point of the whole command, so it lives in one function
// rather than being re-decided at each call site.
func report(c *cobra.Command, r Request, out string) error {
	w := c.OutOrStdout()
	if r.Sink.Kind == SinkStdout {
		fmt.Fprintln(w, out)
		return nil
	}
	if err := warnIfLoud(c, r); err != nil {
		return err
	}
	fmt.Fprintln(w, out)
	return nil
}

// warnIfLoud is where a future noisy-destination notice goes; today every non-
// stdout sink is quiet, so it is a no-op that keeps report's shape honest.
func warnIfLoud(*cobra.Command, Request) error { return nil }

func (r Request) summary() string {
	name := orDash(r.Name)
	return fmt.Sprintf("%-16s %s", name, requesterLabel(r.Requester))
}

func (r Request) state(now time.Time) string {
	switch {
	case hasValue(r.ID):
		return "answered"
	case !r.Pending(now):
		return "expired"
	default:
		return "waiting"
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func absPath(p string) (string, error) {
	abs, err := filepath.Abs(os.ExpandEnv(p))
	if err != nil {
		return "", fmt.Errorf("ask: %s: %w", p, err)
	}
	return abs, nil
}
