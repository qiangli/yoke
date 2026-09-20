// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/svcd"
)

// `bashy app serve` as a supervised daemon, so outpost can keep the console up.
//
// The lifecycle lives one level down — `bashy app service {start,status,stop}`
// — for the same reason meet's does: outpost drives services with
// `bashy <Command...> {start|status|stop}`, and the bare verbs are already
// taken by human-facing commands. `bashy app` opens the console; nothing a
// supervisor should ever call.
//
// WHY OUTPOST SUPERVISES A BASHY TOOL AT ALL. outpost is the host's daemon and
// bashy is the userland; the console is a TOOL, not a service, and being
// supervised does not change that. What outpost supplies is the one thing a
// userland tool cannot supply itself — something that is always up to keep it
// up. The code, the surface and the state stay bashy's.

const serviceSchema = "bashy-apps-service-v1"

// serviceDir keeps the pidfile and log beside the console's own state, so
// relocating $BASHY_HOME relocates the daemon with it and a test can never
// signal a developer's own console.
func serviceDir() (string, error) {
	if h := os.Getenv("BASHY_HOME"); h != "" {
		return filepath.Join(h, "console"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bashy", "console"), nil
}

// spec is the console's daemon identity. Health is otelServiceName because that
// is what GET /healthz reports — it is how stop tells "our console holds this
// port" from "something else does", and therefore what it is allowed to signal.
var spec = svcd.Spec{
	Name:        "apps",
	Schema:      serviceSchema,
	Dir:         serviceDir,
	Argv:        []string{"apps", "serve"},
	Health:      otelServiceName,
	DefaultPort: DefaultPort,
	// The console keeps its loopback listener whatever the LAN one is doing
	// (closed with no paired device, or bound to the symbolic "lan"), so that
	// is where liveness is decided.
	ProbeLoopback: true,
}

func newServiceCmd() *cobra.Command {
	var opt svcd.Options
	var asJSON bool
	var pair bool
	cmd := &cobra.Command{
		Use:   "service",
		Short: "run the console as a supervised background daemon",
		Long: "service is the daemon lifecycle for the console.\n\n" +
			"  bashy app service start    launch the console in the background\n" +
			"  bashy app service status   is it running?\n" +
			"  bashy app service stop     ask it to stop, then insist\n\n" +
			"This is the shape outpost supervises: it runs start, polls status every\n" +
			"30s, and restarts anything that reads stopped. Humans want `bashy app`,\n" +
			"which serves in the foreground and opens the launcher.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().IntVar(&opt.Port, "port", 0, "port the console listens on (0 = default)")
	cmd.PersistentFlags().StringVar(&opt.Bind, "bind", "", "address the console binds (default 127.0.0.1)")
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "emit the status envelope as JSON")
	cmd.PersistentFlags().BoolVar(&pair, "pair", false, "arm QR phone pairing for the console daemon")

	cmd.AddCommand(
		&cobra.Command{
			Use: "start", Short: "start the console daemon",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				effectiveOpt, effectivePair, err := serviceStartPlan(opt, pair,
					c.Flags().Changed("pair") || c.Flags().Changed("bind") || c.Flags().Changed("port"))
				if err != nil {
					return err
				}
				svc := serviceSpec(effectivePair)
				if st, serr := svc.StatusOf(effectiveOpt); serr == nil && st.Running {
					printServiceStatus(c.OutOrStdout(), st, asJSON, "start")
					printServicePairingNotice(c.OutOrStdout(), asJSON)
					return nil
				}
				st, err := svc.Start(effectiveOpt)
				printServiceStatus(c.OutOrStdout(), st, asJSON, "start")
				if err == nil && st.Running {
					if perr := saveServiceProfile(effectiveOpt, effectivePair); perr != nil {
						return perr
					}
				}
				printServicePairingNotice(c.OutOrStdout(), asJSON)
				return err
			},
		},
		&cobra.Command{
			Use: "status", Short: "report whether the console daemon is running",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				st, err := spec.StatusOf(opt)
				printServiceStatus(c.OutOrStdout(), st, asJSON, "status")
				printServicePairingNotice(c.OutOrStdout(), asJSON)
				if err != nil {
					return err
				}
				// The exit code carries the answer too, so a supervisor need not
				// parse prose.
				if !st.Running {
					return svcd.ErrStopped
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "stop", Short: "stop the console daemon",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				st, err := spec.Stop(opt)
				printServiceStatus(c.OutOrStdout(), st, asJSON, "stop")
				return err
			},
		},
	)
	return cmd
}

func serviceSpec(pair bool) svcd.Spec {
	s := spec
	s.Argv = append([]string{}, spec.Argv...)
	if pair {
		s.Argv = append(s.Argv, "--pair")
	}
	return s
}

// printServiceStatus writes the status line outpost greps.
//
// The words "stopped" and "not running" are the supervisor's restart trigger,
// so this text is a wire format with a program on the other end. A running
// console must never print either word.
func printServiceStatus(w io.Writer, st svcd.Status, asJSON bool, action string) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return
	}
	switch {
	case st.Running:
		line := fmt.Sprintf("bashy app: running on %s", st.Addr)
		if st.PID > 0 {
			line += fmt.Sprintf(" (pid %d)", st.PID)
		}
		fmt.Fprintln(w, line)
	case errors.Is(errForState(st), svcd.ErrUnidentified):
		fmt.Fprintf(w, "bashy app: %s\n", st.Detail)
	default:
		fmt.Fprintf(w, "bashy app: stopped\n")
	}
	if st.Detail != "" && st.Running {
		fmt.Fprintf(w, "  %s\n", st.Detail)
	}
	if action == "start" && st.LogFile != "" && st.Running {
		fmt.Fprintf(w, "  log: %s\n", st.LogFile)
	}
}

func printServicePairingNotice(w io.Writer, asJSON bool) {
	if asJSON {
		return
	}
	if notice := servicePairingNotice(); notice != "" {
		fmt.Fprintf(w, "  %s\n", notice)
	}
}

// errForState recovers the sentinel a state implies, so printing does not need
// the error threaded through every branch.
func errForState(st svcd.Status) error {
	if st.State == svcd.StateUnidentified {
		return svcd.ErrUnidentified
	}
	return nil
}
