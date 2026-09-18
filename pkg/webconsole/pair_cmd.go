// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
)

// `bashy app pair` — reach the console from a phone without typing an OS
// password into it.
//
// The flow is three lines of terminal and one scan:
//
//	$ bashy app pair
//	  http://workshop.local:22749/pair/redeem?v=1&t=…     <- also the raw LAN IP
//	  [terminal QR]
//	  waiting 120s for a device…
//
// FIVE THINGS THAT MATTER, in the order they bite:
//
//  1. The ticket is single-use and the window is short (120s by default).
//     Both close on first redemption.
//  2. A device session inherits but never exceeds the operator authority that
//     minted it: TTL is min(--ttl, remaining grant), and `revoke --all` ends
//     the grant, which ends every device derived from it.
//  3. The default SCOPE is the board, the message board and the room — not
//     the terminal, not the home
//     directory. `--allow terminal` opts in explicitly.
//  4. mDNS is best-effort. macOS and Windows answer <host>.local natively,
//     Linux needs avahi — so the raw LAN IP is always printed too and an
//     unresolvable .local is cosmetic.
//  5. It is HTTP, and this says so: pairing stops the OS PASSWORD from
//     crossing the wire, it does not make the link private. The device cookie
//     is still sniffable on a plaintext LAN. Fine for a home network; not for
//     a café's.

func newPairCmd() *cobra.Command {
	var (
		ttl    time.Duration
		window time.Duration
		allow  []string
		host   string
		port   int
		asJSON bool
		noWait bool
		noQR   bool
	)
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "pair a phone with a QR code, so an OS password never crosses the wire",
		Long: "pair mints a single-use, time-boxed pairing ticket and prints it as a QR.\n\n" +
			"Scanning it opens the console on the phone with a DEVICE-SCOPED session:\n" +
			"Sprint, Messages and Meet by default — not a shell, not your home directory.\n" +
			"Use --allow to widen it deliberately.\n\n" +
			"Honest about the wire: this stops your OS PASSWORD from crossing a\n" +
			"plaintext LAN. It does not encrypt the link.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runPair(c.Context(), c.OutOrStdout(), pairOptions{
				TTL: ttl, Window: window, Allow: allow,
				Host: host, Port: port, JSON: asJSON,
				NoWait: noWait, NoQR: noQR,
			})
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", defaultDeviceTTL,
		"how long the paired device keeps access (capped by the operator session)")
	cmd.Flags().DurationVar(&window, "window", defaultTicketWindow,
		"how long the code stays scannable")
	cmd.Flags().StringSliceVar(&allow, "allow", nil,
		"panels the device may reach, by app name or mount (default sprint,mb,meet; NOT terminal or files)")
	cmd.Flags().StringVar(&host, "host", "", "address the phone should dial (default: this host's LAN IP)")
	cmd.Flags().IntVar(&port, "port", DefaultPort, "port the console listens on")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the ticket as a typed object instead of a QR")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "print the code and exit instead of waiting for a scan")
	cmd.Flags().BoolVar(&noQR, "no-qr", false, "print the URL only, no QR block")
	return cmd
}

type pairOptions struct {
	TTL    time.Duration
	Window time.Duration
	Allow  []string
	Host   string
	Port   int
	JSON   bool
	NoWait bool
	NoQR   bool
}

// pairEnvelope is the --json shape: typed data, never a rendered table.
type pairEnvelope struct {
	Schema     string    `json:"schema"`
	TicketID   string    `json:"ticket_id"`
	URL        string    `json:"url"`
	LANURL     string    `json:"lan_url,omitempty"`
	MDNSURL    string    `json:"mdns_url,omitempty"`
	Scope      []string  `json:"scope"`
	DeviceTTL  string    `json:"device_ttl"`
	Expires    time.Time `json:"expires"`
	PayloadVer string    `json:"payload_version"`
	Encrypted  bool      `json:"encrypted"`
	Note       string    `json:"note"`
}

func runPair(ctx context.Context, out io.Writer, opt pairOptions) error {
	store, err := openPairStore()
	if err != nil {
		return err
	}
	panels := Discover()
	if err := ValidateScope(opt.Allow, panels); err != nil {
		return fmt.Errorf("apps pair: %w", err)
	}
	scope := normalizeScope(opt.Allow)

	lanIP := opt.Host
	if lanIP == "" {
		lanIP = primaryLANAddr()
	}
	if lanIP == "" {
		return fmt.Errorf("apps pair: could not work out this host's LAN address; pass --host <ip>")
	}

	t, secret, err := store.issueTicket(scope, opt.TTL, opt.Window)
	if err != nil {
		return err
	}
	lanURL := redeemURL(lanIP, opt.Port, secret)
	mdnsURL := ""
	// An explicit --host is an ADDRESS THE OPERATOR CHOSE. Preferring a
	// guessed <host>.local over it would hand back a QR pointing somewhere
	// they did not ask for — and the mismatch only shows up as a cookie that
	// silently does not apply, because a session cookie is scoped to the host
	// it was set on.
	if opt.Host == "" {
		if h := mdnsName(); h != "" {
			mdnsURL = redeemURL(h, opt.Port, secret)
		}
	}
	primary := mdnsURL
	if primary == "" {
		primary = lanURL
	}

	auditPairEvent("pair.issued", map[string]string{
		"ticket": t.ID, "scope": strings.Join(scope, ","),
		"device_ttl": t.TTL, "expires": t.Expires.Format(time.RFC3339),
	})

	if opt.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(pairEnvelope{
			Schema: "bashy-apps-pair-v1", TicketID: t.ID,
			URL: primary, LANURL: lanURL, MDNSURL: mdnsURL,
			Scope: scope, DeviceTTL: t.TTL, Expires: t.Expires,
			PayloadVer: pairQRVersion, Encrypted: false,
			Note: "plaintext HTTP: the pairing stops the OS password from crossing the wire, it does not encrypt the link",
		}); err != nil {
			return err
		}
		if opt.NoWait {
			return nil
		}
		return waitForScan(ctx, out, store, t, false)
	}

	fmt.Fprintf(out, "bashy app pair — scan with the phone's camera\n\n")
	if !opt.NoQR {
		block, qerr := terminalQR(primary)
		if qerr != nil {
			fmt.Fprintf(out, "  (could not render a QR: %v)\n", qerr)
		} else {
			fmt.Fprintln(out, block)
		}
	}
	if mdnsURL != "" {
		fmt.Fprintf(out, "  %s\n", mdnsURL)
		fmt.Fprintf(out, "  fallback (if .local does not resolve):\n  %s\n", lanURL)
	} else {
		fmt.Fprintf(out, "  %s\n", lanURL)
	}
	fmt.Fprintf(out, "\n  scope        %s\n", strings.Join(scope, ", "))
	fmt.Fprintf(out, "  device TTL   %s\n", t.TTL)
	fmt.Fprintf(out, "  code expires %s (single use)\n", t.Expires.Local().Format("15:04:05"))
	fmt.Fprintf(out, "\n  This stops your OS password from crossing the LAN. It does NOT\n")
	fmt.Fprintf(out, "  encrypt the link — the device cookie is still sniffable on a\n")
	fmt.Fprintf(out, "  plaintext network. Fine at home; not on a shared wifi.\n\n")

	if opt.NoWait {
		return nil
	}
	return waitForScan(ctx, out, store, t, true)
}

// waitForScan polls the store until the ticket is redeemed or its window
// closes. Polling rather than a channel because the redeeming process is the
// SERVER, not this one — the file is the only thing they share.
func waitForScan(ctx context.Context, out io.Writer, store *pairStore, t ticket, verbose bool) error {
	if verbose {
		fmt.Fprintf(out, "  waiting %s for a scan… (Ctrl-C to stop; the code still works until it expires)\n",
			time.Until(t.Expires).Round(time.Second))
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		st, err := store.load()
		if err != nil {
			return err
		}
		for _, cur := range st.Tickets {
			if cur.ID != t.ID || cur.Redeemed == nil {
				continue
			}
			d, _ := st.findDevice(cur.DeviceID, time.Now())
			fmt.Fprintf(out, "\n  paired: %s (%s)\n", d.Name, d.ID)
			fmt.Fprintf(out, "  scope:  %s\n", strings.Join(d.Scope, ", "))
			fmt.Fprintf(out, "  until:  %s\n", d.Expires.Local().Format(time.RFC1123))
			fmt.Fprintf(out, "  revoke: bashy app revoke %s\n", d.ID)
			return nil
		}
		if !time.Now().Before(t.Expires) {
			auditPairEvent("pair.expired", map[string]string{"ticket": t.ID})
			return fmt.Errorf("apps pair: the code expired before it was scanned (window %s; raise it with --window)",
				t.Expires.Sub(t.Issued).Round(time.Second))
		}
	}
}

// redeemURL builds the versioned payload the QR carries.
//
// It is a plain URL rather than a custom scheme so a phone's stock camera
// opens it directly, and it carries an explicit `v` so a v2 payload — one
// carrying a self-signed certificate fingerprint for the phone to pin — is
// refused by an older console instead of being half-understood.
func redeemURL(host string, port int, secret string) string {
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, fmt.Sprint(port)),
		Path:     pairRedeemPath,
		RawQuery: url.Values{"v": {pairQRVersion}, "t": {secret}}.Encode(),
	}
	return u.String()
}

// terminalQR renders a QR as half-block characters.
func terminalQR(payload string) (string, error) {
	q, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return "", err
	}
	q.DisableBorder = false
	return q.ToSmallString(false), nil
}

// primaryLANAddr finds this host's outward-facing address WITHOUT sending a
// packet: a UDP "connection" only resolves a route, so this works offline and
// costs nothing.
func primaryLANAddr() string {
	c, err := net.Dial("udp4", "192.0.2.1:9") // TEST-NET-1, deliberately unroutable
	if err == nil {
		defer c.Close()
		if a, ok := c.LocalAddr().(*net.UDPAddr); ok && a.IP != nil && !a.IP.IsLoopback() {
			return a.IP.String()
		}
	}
	// Fall back to the first non-loopback IPv4 on an up interface.
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if v4 := ipn.IP.To4(); v4 != nil && !v4.IsLoopback() {
					return v4.String()
				}
			}
		}
	}
	return ""
}

// mdnsName is <hostname>.local, or "" when the host name is unusable.
// Best-effort by design — see the header comment.
func mdnsName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	h = strings.TrimSuffix(h, ".")
	if i := strings.IndexByte(h, '.'); i >= 0 {
		// Already qualified (foo.local, foo.lan): use it as-is.
		return h
	}
	return h + ".local"
}

// ---------------------------------------------------------------------------
// devices / revoke
// ---------------------------------------------------------------------------

func newDevicesCmd() *cobra.Command {
	var asJSON, all bool
	cmd := &cobra.Command{
		Use:           "devices",
		Short:         "list paired devices, what each one may reach, and when it expires",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			store, err := openPairStore()
			if err != nil {
				return err
			}
			st, err := store.load()
			if err != nil {
				return err
			}
			now := time.Now()
			list := st.Devices
			if !all {
				list = st.liveDevices(now)
			}
			if asJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				out := make([]map[string]any, 0, len(list))
				for _, d := range list {
					out = append(out, map[string]any{
						"id": d.ID, "name": d.Name, "scope": d.Scope,
						"issued": d.Issued, "expires": d.Expires,
						"last_seen": d.LastSeen, "user_agent": d.UserAgent,
						"live": st.deviceLive(d, now), "revoked": d.Revoked,
					})
				}
				return enc.Encode(out)
			}
			if len(list) == 0 {
				fmt.Fprintln(c.OutOrStdout(), "no paired devices (pair one with: bashy app pair)")
				return nil
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSCOPE\tSTATE\tEXPIRES\tLAST SEEN")
			for _, d := range list {
				state := "live"
				switch {
				case d.Revoked:
					state = "revoked"
				case !now.Before(d.Expires):
					state = "expired"
				case !st.deviceLive(d, now):
					state = "grant-ended"
				}
				seen := "never"
				if !d.LastSeen.IsZero() {
					seen = d.LastSeen.Local().Format("15:04:05")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					d.ID, d.Name, strings.Join(d.Scope, ","), state,
					d.Expires.Local().Format("15:04:05"), seen)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit typed device objects instead of a table")
	cmd.Flags().BoolVar(&all, "all", false, "include revoked and expired devices")
	return cmd
}

func newRevokeCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "revoke [device-id]",
		Short: "end one paired device, or --all to end the operator grant behind every one",
		Long: "revoke ends a device's access immediately — the next request it makes is\n" +
			"refused and its cookie is cleared.\n\n" +
			"--all ends the OPERATOR GRANT that every pairing hangs off, which ends\n" +
			"every device derived from it in one write. That is the same authority a\n" +
			"login gives, so ending it is the honest meaning of \"sign everything out\".",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			store, err := openPairStore()
			if err != nil {
				return err
			}
			if all {
				n, err := store.revokeAll()
				if err != nil {
					return err
				}
				auditPairEvent("pair.revoked_all", map[string]string{"devices": fmt.Sprint(n)})
				fmt.Fprintf(c.OutOrStdout(), "bashy app: revoked the operator grant and %d device(s)\n", n)
				return nil
			}
			if len(args) != 1 {
				return fmt.Errorf("apps revoke: name one device id, or pass --all (see `bashy app devices`)")
			}
			found, err := store.revoke(args[0])
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("apps revoke: no live device %q (see `bashy app devices --all`)", args[0])
			}
			auditPairEvent("pair.revoked", map[string]string{"device": args[0]})
			fmt.Fprintf(c.OutOrStdout(), "bashy app: revoked %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "revoke every device by ending the operator grant")
	return cmd
}
