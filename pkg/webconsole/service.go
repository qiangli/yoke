// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

// `bashy app serve` is a foreground process; the seeded `apps` service unit
// declares how a monitor runs it (docs/bashy-service-design.md). The lifecycle
// verbs below hold no daemon, pidfile or profile of their own: they write the
// unit's desired state and listener/settings through the generic `service`
// verbs and report the unit's state in the monitor's service.Status shape.
//
// `app service` stays one level down from the bare verbs for the same reason
// as before: `bashy app` opens the console for a human.

// serviceUnit is the console's own unit record.
const serviceUnit = "apps"

// errInactive is the exit status of `service status` for a unit that is not
// serving, so a caller need not parse text.
var errInactive = errors.New("apps: service is not active")

// serviceDir is the console's state directory (session key, pairing, look).
// Relocating $BASHY_HOME relocates it, so a test can never touch a
// developer's own console.
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

// servicePlan is the launch shape carried by the unit: the primary listener
// and the `pair` / `launcher` settings its exec expands.
type servicePlan struct {
	Port     int
	Bind     string
	Pair     bool
	Launcher string
}

func planOf(u fleet.Service) servicePlan {
	p := servicePlan{Port: DefaultPort, Bind: "127.0.0.1", Pair: u.Settings["pair"] == "true", Launcher: u.Settings["launcher"]}
	if lp, ok := u.PrimaryPort(); ok {
		p.Port = lp.Port
		if lp.Bind != "" {
			p.Bind = lp.Bind
		}
	}
	return p
}

// setArgs is the `service set` argument vector that makes the unit match p.
func (p servicePlan) setArgs() []string {
	return []string{"set", serviceUnit,
		"--set", "ports.0.port=" + strconv.Itoa(p.Port),
		"--set", "ports.0.bind=" + p.Bind,
		"--set", "settings.pair=" + strconv.FormatBool(p.Pair),
		"--set", "settings.launcher=" + p.Launcher,
	}
}

func loadServiceUnit() (fleet.Service, error) {
	u, ok := fleet.New().Service(serviceUnit)
	if !ok {
		return fleet.Service{}, fmt.Errorf("apps: no %q service unit", serviceUnit)
	}
	return u, nil
}

// runServiceVerb runs one generic `bashy service` verb in-process, so the
// write answers to every rule `bashy service` applies (validation, port
// conflicts, sparse overlay).
func runServiceVerb(ctx context.Context, errw io.Writer, args ...string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := fleet.NewServiceCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(errw)
	return cmd.ExecuteContext(ctx)
}

func newServiceCmd() *cobra.Command {
	var port int
	var bind, launcher string
	var asJSON, pair bool
	cmd := &cobra.Command{
		Use:   "service",
		Short: "set the console service unit's desired state and report its status",
		Long: "service drives the `apps` service unit.\n\n" +
			"  bashy app service start    enable the unit (with the given listener/pairing)\n" +
			"  bashy app service status   the unit's state, as service.Status\n" +
			"  bashy app service stop     disable the unit\n\n" +
			"The console itself runs in the foreground (`bashy app serve`); a running\n" +
			"`bashy supervisord` starts and stops it as the unit's desired state changes.\n" +
			"These verbs are the same writes as `bashy service set|enable|disable apps`.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.PersistentFlags().StringVar(&launcher, "launcher", "", "serve a directory containing index.html as the launcher")
	cmd.PersistentFlags().IntVar(&port, "port", 0, "port the console listens on (0 = default)")
	cmd.PersistentFlags().StringVar(&bind, "bind", "", "address the console binds (default 127.0.0.1)")
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "emit the status as JSON")
	cmd.PersistentFlags().BoolVar(&pair, "pair", false, "arm QR phone pairing for the console")

	cmd.AddCommand(
		&cobra.Command{
			Use: "start", Short: "enable the console unit",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				unit, err := loadServiceUnit()
				if err != nil {
					return err
				}
				current := planOf(unit)
				explicit := c.Flags().Changed("pair") || c.Flags().Changed("bind") || c.Flags().Changed("port")
				want, err := serviceStartPlan(current, servicePlan{Port: port, Bind: bind, Pair: pair}, explicit)
				if err != nil {
					return err
				}
				want.Launcher, err = serviceLauncherPlan(current.Launcher, launcher, c.Flags().Changed("launcher"))
				if err != nil {
					return err
				}
				if want != current {
					if err := runServiceVerb(c.Context(), c.ErrOrStderr(), want.setArgs()...); err != nil {
						return err
					}
				}
				if err := runServiceVerb(c.Context(), c.ErrOrStderr(), "enable", serviceUnit); err != nil {
					return err
				}
				return reportService(c.OutOrStdout(), asJSON, "start")
			},
		},
		&cobra.Command{
			Use: "status", Short: "report the console unit's state",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				st, err := serviceStatus(c.Context())
				if err != nil {
					return err
				}
				printServiceStatus(c.OutOrStdout(), st, asJSON, "status")
				printServicePairingNotice(c.OutOrStdout(), asJSON)
				if st.State != service.Active {
					return errInactive
				}
				return nil
			},
		},
		&cobra.Command{
			Use: "stop", Short: "disable the console unit",
			SilenceUsage: true, SilenceErrors: true,
			RunE: func(c *cobra.Command, _ []string) error {
				if err := runServiceVerb(c.Context(), c.ErrOrStderr(), "disable", serviceUnit); err != nil {
					return err
				}
				return reportService(c.OutOrStdout(), asJSON, "stop")
			},
		},
	)
	return cmd
}

func reportService(w io.Writer, asJSON bool, action string) error {
	st, err := serviceStatus(context.Background())
	if err != nil {
		return err
	}
	printServiceStatus(w, st, asJSON, action)
	printServicePairingNotice(w, asJSON)
	return nil
}

// serviceStatus is the unit's state: desired state from the declaration, and
// liveness from the unit's own http health probe, which is how a monitor
// decides Active. /healthz reports otelServiceName, so a port held by
// something else is Failed rather than mistaken for the console. The console
// keeps its loopback listener whatever the LAN one is doing, so that is where
// liveness is decided.
func serviceStatus(ctx context.Context) (service.Status, error) {
	unit, err := loadServiceUnit()
	if err != nil {
		return service.Status{}, err
	}
	p := planOf(unit)
	st := service.Status{
		Unit: unit.Name, Provider: unit.ProviderName(), Enabled: unit.Enabled,
		Ports: []string{p.Bind + ":" + strconv.Itoa(p.Port) + "/tcp"},
	}
	switch probeConsole(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Port))) {
	case probeServing:
		st.State, st.Health = service.Active, "ok"
		if !unit.Enabled {
			st.Reason = "serving while disabled; a running monitor stops it"
		}
	case probeForeign:
		st.State = service.Failed
		st.Health = "unidentified listener"
		st.Reason = fmt.Sprintf("port %d is in use by an unidentified listener", p.Port)
	default:
		st.State = service.Inactive
		if unit.Enabled {
			st.Reason = "enabled; nothing is serving (is bashy supervisord running?)"
		} else {
			st.Reason = "disabled"
		}
	}
	return st, nil
}

type consoleProbe int

const (
	probeDown consoleProbe = iota
	probeServing
	probeForeign
)

func probeConsole(ctx context.Context, addr string) consoleProbe {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return probeDown
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return probeDown
	}
	defer resp.Body.Close()
	var body struct {
		Service string `json:"service"`
	}
	if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil &&
		body.Service == otelServiceName {
		return probeServing
	}
	return probeForeign
}

func printServiceStatus(w io.Writer, st service.Status, asJSON bool, action string) {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(st)
		return
	}
	line := fmt.Sprintf("bashy app: %s", st.State)
	if len(st.Ports) > 0 {
		line += " on " + st.Ports[0]
	}
	if !st.Enabled {
		line += " (disabled)"
	}
	fmt.Fprintln(w, line)
	if st.Reason != "" {
		fmt.Fprintf(w, "  %s\n", st.Reason)
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
