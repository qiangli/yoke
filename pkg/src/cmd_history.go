package src

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/spf13/cobra"
)

// userFlag registers --user with --principal as its alias: both write the
// same principal, from the flag or $BASHY_PRINCIPAL.
func userFlag(c *cobra.Command, v *string) {
	c.Flags().StringVar(v, "user", "", "web principal to act for (default $"+principalEnv+")")
	c.Flags().StringVar(v, "principal", "", "alias for --user")
}

func resolveUser(v string) (string, error) {
	p := strings.TrimSpace(v)
	if p == "" {
		p = strings.TrimSpace(os.Getenv(principalEnv))
	}
	if p == "" {
		return "", fmt.Errorf("src: this verb needs --user or $%s", principalEnv)
	}
	return p, nil
}

func newHistoryCmd() *cobra.Command {
	var user string
	var asJSON bool
	c := &cobra.Command{
		Use:   "history <app>",
		Short: "List a principal's version history: who, when, request, verdict, evidence",
		Long: `history lists every version on the principal's version line,
newest first: who recorded it, when, its version (the semver release tag,
or a pseudo-version when unreleased), the request text, the verifier
verdict, and the evidence reference, followed by the named labels, review
versions, and the app's releases. The principal comes from --user or
$BASHY_PRINCIPAL.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			h, err := History("", args[0], p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, err := json.MarshalIndent(h, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(b))
				return nil
			}
			fmt.Fprintf(out, "app: %s\nprincipal: %s\nline: %s\n", h.App, h.Principal, h.Line)
			if h.Tip != "" {
				fmt.Fprintf(out, "tip: %s\n", h.Tip)
			}
			if h.Active != "" {
				fmt.Fprintf(out, "active: %s\n", h.Active)
			}
			if h.Base != "" {
				fmt.Fprintf(out, "base: %s\n", h.Base)
			}
			if len(h.Entries) == 0 {
				fmt.Fprintf(out, "(no versions on %s)\n", h.Line)
			}
			for _, e := range h.Entries {
				fmt.Fprintf(out, "version %s %s rev %s %s %s\n",
					shortSHA(e.Activation), e.Kind, shortSHA(e.Revision),
					e.When.Format("2006-01-02T15:04:05Z07:00"), e.Author)
				if e.Version != "" {
					fmt.Fprintf(out, "  version: %s\n", e.Version)
				}
				fmt.Fprintf(out, "  request: %s\n", e.Request)
				fmt.Fprintf(out, "  verdict: %s\n", e.Verdict)
				fmt.Fprintf(out, "  evidence: %s\n", e.Evidence)
			}
			if len(h.Named) > 0 {
				fmt.Fprintln(out, "named:")
				for _, n := range h.Named {
					fmt.Fprintf(out, "  %s -> %s\n", n.Name, shortSHA(n.Revision))
				}
			}
			if len(h.Reviews) > 0 {
				fmt.Fprintln(out, "review:")
				for _, n := range h.Reviews {
					fmt.Fprintf(out, "  %s -> %s\n", n.ID, shortSHA(n.Revision))
				}
			}
			if len(h.Releases) > 0 {
				fmt.Fprintln(out, "releases:")
				for _, rt := range h.Releases {
					fmt.Fprintf(out, "  %s -> %s\n", rt.Version, shortSHA(rt.Revision))
				}
			}
			return nil
		},
	}
	userFlag(c, &user)
	c.Flags().BoolVar(&asJSON, "json", false, "print the history as JSON")
	return c
}

func newPreviewCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "preview <app> <rev>",
		Short: "Check a version out in an isolated workspace without moving the active revision",
		Long: `preview prepares an isolated workspace at the named version
(a commit, a named label, or base) and checks it out there. The app
checkout, the version line, and the active revision do not move.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := strings.TrimSpace(user)
			if p == "" {
				p = strings.TrimSpace(os.Getenv(principalEnv))
			}
			dir, err := Preview("", args[0], p, args[1])
			if err != nil {
				return err
			}
			active := "(none)"
			if store, err := state.OpenAt(Home(), args[0], p); err == nil {
				if st, err := store.Read(); err == nil && st.ActiveRevision != "" {
					active = shortSHA(st.ActiveRevision)
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "preview ready at %s\nopen it with: cd %s\nactive revision unchanged (%s)\n",
				dir, dir, active)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}

func newNameCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "name <app> <rev> <label>",
		Short: "Pin a label to a version under the principal's namespace",
		Long: `name pins a label to a version so history, preview, diff, and
restore can name it instead of spelling a commit. The principal comes
from --user or $BASHY_PRINCIPAL.`,
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			ref, err := Name("", args[0], p, args[1], args[2])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "named %s (%s)\n", args[2], ref)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}

func newRestoreCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "restore <app> <rev>",
		Short: "Return to a version through a new forward commit, gated and activated",
		Long: `restore returns to a version (a version-line activation
commit, a revision, a named label, or base) by verifying it against the
current line tip through the trusted verifier, sealing fresh evidence,
and recording a NEW forward commit on the principal's version line whose
tree equals that version's, passing it through the activation gate, and
activating it. History is never rewritten: undoing a restore is
restoring the pre-restore version. Only code and config move; the
principal data directory is untouched, and a target that cannot read the
current data schema, a verifier refusal, or a gate refusal leaves the
active revision unchanged. The principal comes from --user or
$BASHY_PRINCIPAL.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			res, err := RestoreSealed(cmd.Context(), "", args[0], args[1], p, RestoreSealedOptions{})
			if err != nil {
				return err
			}
			prev := "(none)"
			if res.Previous != "" {
				prev = shortSHA(res.Previous)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s for %s at %s (was %s)\n",
				res.App, res.Principal, shortSHA(res.Revision), prev)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}

func newUndoCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "undo <app>",
		Short: "Return to the previous active revision through a new gated commit",
		Long: `undo restores the principal's previous active revision (the
last-known-good the last activation or restore recorded) by recording a
NEW forward commit on the principal's version line, passing it through
the activation gate, and activating it. History is never rewritten: the
undo of a restore is a restore. With nothing to undo it refuses and the
active revision stays where it is. The principal comes from --user or
$BASHY_PRINCIPAL.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			res, err := UndoSealed(cmd.Context(), "", args[0], p, RestoreSealedOptions{})
			if err != nil {
				return err
			}
			prev := "(none)"
			if res.Previous != "" {
				prev = shortSHA(res.Previous)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restored %s for %s at %s (was %s)\n",
				res.App, res.Principal, shortSHA(res.Revision), prev)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}
