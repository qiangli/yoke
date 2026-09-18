// Package tessaro is the account / front-door for the dhnt system: `bashy tessaro`
// (login/logout/status/open) and the top-level shortcut `bashy login`. Tessaro is
// the hosted control plane at tessaro.sh; a machine joins it by pairing via the
// outpost mesh agent. Like `bashy sphere`, these verbs exec the agent at runtime
// (external/meshagent) with ZERO build dependency on outpost, so bashy stays the
// standalone keystone. `bashy tessaro open` works even without the agent.
package tessaro

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/meshagent"
)

// PortalURL is the Tessaro front door.
const PortalURL = "https://tessaro.sh"

var errHandled = errors.New("tessaro: handled")

// subVerbs maps `bashy tessaro <verb>` to the outpost account subcommand.
var subVerbs = map[string]string{
	"login":   "register", // pair this machine with the portal (sign in)
	"signin":  "register",
	"pair":    "register",
	"logout":  "unpair", // clear the portal pairing (sign out)
	"signout": "unpair",
	"unpair":  "unpair",
	"status":  "status", // pairing + built-ins + outbound state
	"whoami":  "status",
}

// NewTessaroCmd builds `bashy tessaro`: the account front-door.
func NewTessaroCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "tessaro",
		Short:              "Tessaro account: sign in / out, status, open the portal (via the outpost agent)",
		Long:               tessaroLong,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
				fmt.Fprint(cmd.OutOrStdout(), tessaroLong, "\n")
				return nil
			}
			sub := args[0]
			rest := args[1:]
			if sub == "open" { // works without the agent — just open the portal
				return openPortal(cmd)
			}
			outSub, ok := subVerbs[sub]
			if !ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "tessaro: unknown subcommand %q — try: login, logout, status, open\n", sub)
				return errHandled
			}
			return runAgent(cmd, append([]string{outSub}, rest...)...)
		},
	}
}

// NewLoginCmd is the top-level shortcut `bashy login` = `bashy tessaro login`.
func NewLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Sign in to Tessaro — pair this machine with the portal (via the outpost agent)",
		Long: `login pairs this machine with Tessaro (the hosted control plane) so it can join
your mesh — the same as ` + "`bashy tessaro login`" + `. Run bare for the interactive
pairing prompt, or pass --code <invite> (from the portal's "Add a machine" dialog).
Everything after ` + "`login`" + ` passes through to the outpost pairing flow.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgent(cmd, append([]string{"register"}, args...)...)
		},
	}
}

// runAgent execs the outpost account subcommand, printing the inviting join
// guidance when the agent isn't installed yet.
func runAgent(cmd *cobra.Command, args ...string) error {
	err := meshagent.Exec(cmd.Context(), args...)
	if errors.Is(err, meshagent.ErrNotFound) {
		fmt.Fprintln(cmd.ErrOrStderr(), joinLines)
		return errHandled
	}
	return err
}

// openPortal opens tessaro.sh in the default browser (best-effort; prints the URL
// if no opener is available).
func openPortal(cmd *cobra.Command) error {
	var opener string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		opener, args = "open", []string{PortalURL}
	case "windows":
		opener, args = "rundll32", []string{"url.dll,FileProtocolHandler", PortalURL}
	default:
		opener, args = "xdg-open", []string{PortalURL}
	}
	if path, err := exec.LookPath(opener); err == nil {
		c := exec.CommandContext(cmd.Context(), path, args...)
		if err := c.Start(); err == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "opening "+PortalURL)
			return nil
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), PortalURL)
	return nil
}

var tessaroLong = strings.Join([]string{
	"Tessaro is the front door to your dhnt mesh — pooled LLMs + durable agentic",
	"fleets across the computers you own, cloud as a thin relay. This machine joins",
	"it by pairing via the outpost agent.",
	"",
	"  bashy login              sign in — pair this machine (= tessaro login)",
	"  bashy tessaro login      " + PortalURL + " pairing (--code <invite> to skip prompts)",
	"  bashy tessaro logout     clear the pairing (sign out)",
	"  bashy tessaro status     pairing + built-ins + outbound state",
	"  bashy tessaro open       open " + PortalURL + " in your browser",
	"",
	"Set $OUTPOST_BIN to point at a specific outpost binary.",
}, "\n")

// joinLines invites a not-yet-connected machine to Tessaro, then names how to
// pair once the agent is installed.
var joinLines = strings.Join([]string{
	"tessaro: this machine isn't connected to Tessaro yet.",
	"",
	"Tessaro is the front door to your dhnt mesh — pooled LLMs + agentic fleets",
	"across the computers you own, cloud as a thin relay.",
	"",
	"    1. Sign up / sign in:   " + PortalURL,
	"    2. Add this machine     (downloads the Tessaro agent 'outpost' + pairs it)",
	"",
	"Then `bashy login` pairs this host and `bashy tessaro status` shows it.",
	"(Already have the agent? Put 'outpost' on PATH or set $OUTPOST_BIN.)",
}, "\n")
