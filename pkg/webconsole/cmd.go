// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/qiangli/yoke/pkg/hostauth"
	"github.com/qiangli/yoke/pkg/websession"
)

// NewAppsCmd is the `bashy app` tree.
func NewAppsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "open bashy's apps in a browser: Terminal, Files, Meet, and every declared surface",
		Long: "app serves bashy's Apps — its surfaces in a browser at one address.\n\n" +
			"It is ONE launcher with the apps deep-linked beneath it, not one server per\n" +
			"verb: one nav, one auth, one design system. `bashy commands --view web` lists\n" +
			"the same surfaces in the terminal.",
		SilenceUsage: true,
		// The caller (bashy's dispatch arm) prints the error. Without this cobra
		// prints its own copy first and every failure is reported twice.
		SilenceErrors: true,
	}
	cmd.AddCommand(newServeCmd(), newListCmd(), newServiceCmd(),
		newPairCmd(), newDevicesCmd(), newRevokeCmd())
	// Bare `bashy app` serves — the common case should not need a subcommand.
	cmd.RunE = func(c *cobra.Command, args []string) error {
		serve, _, err := c.Find([]string{"serve"})
		if err != nil {
			return err
		}
		return serve.RunE(serve, args)
	}
	return cmd
}

// sessionKey returns the persistent HMAC key for console sessions, creating it
// on first use with 0600 permissions.
func sessionKey() ([]byte, error) {
	dir := os.Getenv("BASHY_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(home, ".bashy")
	}
	dir = filepath.Join(dir, "console")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "session.key")
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func newServeCmd() *cobra.Command {
	var (
		port    int
		bind    string
		scope   string
		write   bool
		pair    bool
		disable []string
		apps    []string
		appAuth []string
	)
	cmd := &cobra.Command{
		Use:           "serve",
		Short:         "serve the console",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			auth, err := ParseAppAuth(appAuth)
			if err != nil {
				return err
			}
			return runServe(c.Context(), c.OutOrStdout(), Options{
				Scope:      scope,
				AllowWrite: write,
				Pairing:    pair,
				Disable:    disable,
				Apps:       apps,
				AppAuth:    auth,
			}, bind, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", DefaultPort, "port to listen on")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "address to bind: an IP, or `lan` for the host's current primary LAN address (followed as the network changes)")
	cmd.Flags().StringVar(&scope, "scope", "", "filesystem root for the files panel (default: your home directory)")
	cmd.Flags().BoolVar(&write, "allow-write", false, "allow the files panel to modify files")
	cmd.Flags().StringSliceVar(&disable, "disable", nil,
		"panels to leave out entirely — neither listed nor routed (terminal,files,meet,mb,sprint)")
	cmd.Flags().StringArrayVar(&apps, "app", nil,
		"publish a third-party program as a tile: <bin> or <bin>@<port>, repeatable "+
			"(described by `<bin> meta --json`)")
	cmd.Flags().StringArrayVar(&appAuth, "app-auth", nil,
		"operator-authorized auth tier for a third-party mount: <mount>=public|system|custom, repeatable")
	cmd.Flags().BoolVar(&pair, "pair", false,
		"accept QR device pairings (`bashy app pair`), and keep the LAN listener open "+
			"only while a paired device exists")
	return cmd
}

func newListCmd() *cobra.Command {
	var apps, appAuth []string
	cmd := &cobra.Command{
		Use:           "list",
		Short:         "list the apps and whether each one is up",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			auth, err := ParseAppAuth(appAuth)
			if err != nil {
				return err
			}
			var pc probeCache
			panels := Discover()
			if len(apps) > 0 {
				extra, errs := discoverApps(c.Context(), apps, nil, TakenMounts(panels), auth)
				for _, err := range errs {
					fmt.Fprintf(c.ErrOrStderr(), "apps: skipping --app: %v\n", err)
				}
				panels = append(panels, extra...)
			}
			w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SURFACE\tPATH\tMODE\tAUTH\tSOURCE\tSTATUS\tSTART")
			for _, st := range pc.Probe(c.Context(), panels) {
				note := st.StartHint
				if st.Status == StatusUnavailable {
					note = st.Note
				}
				auth := st.Auth
				if auth == "" {
					auth = AuthSystem
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					st.Name, st.Path, st.Mode, auth, st.Source, st.Status, note)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringArrayVar(&apps, "app", nil,
		"publish a third-party program as a tile: <bin> or <bin>@<port>, repeatable")
	cmd.Flags().StringArrayVar(&appAuth, "app-auth", nil,
		"operator-authorized auth tier for a third-party mount: <mount>=public|system|custom, repeatable")
	return cmd
}

// runServe binds and serves. The bind precondition is checked BEFORE listening,
// so an operator who asks for something unsafe learns at the point of the
// mistake rather than from a 403 later.
func runServe(ctx context.Context, out io.Writer, opts Options, bind string, port int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	addr := net.JoinHostPort(bind, strconv.Itoa(port))

	// Binding off-loopback exposes this host's shell and files, so it requires a
	// system login. The check is at BIND time, not per request: an operator who
	// asks for something that cannot be made safe should learn at the point of
	// the mistake, not from a 403 an hour later.
	offLoopback := !coopauth.IsLoopbackAddr(addr)
	if offLoopback {
		auth := hostauth.DefaultAuthenticator()
		// Probe with empty credentials: every backend reports "I work, and no,
		// that is not a password" without needing a real secret.
		if err := auth.Authenticate("", ""); !errors.Is(err, hostauth.ErrInvalidCredentials) {
			return fmt.Errorf("apps: --bind %s exposes this host's shell and files "+
				"off-loopback, which requires a system login, and host authentication "+
				"is not available here (%w).\nBind 127.0.0.1, or reach it through outpost",
				bind, err)
		}
		opts.RequireLogin = true

		// Persist the signing key so a restart does not sign everyone out.
		key, err := sessionKey()
		if err != nil {
			return fmt.Errorf("apps: session key: %w", err)
		}
		opts.Sessions = websession.NewStore(12*time.Hour, key)
	} else if opts.Pairing {
		// A pairing ticket names a LAN address. Minting one against a console
		// nothing on the LAN can reach would hand the operator a QR that
		// cannot work, and they would learn it from the phone.
		return fmt.Errorf("apps: --pair needs a LAN-bound console; " +
			"start it with --bind lan (or an explicit LAN IP), " +
			"or reach this host through outpost instead")
	}

	opts.Ctx = ctx
	// The QR a Settings-page pairing mints must point a phone at the port this
	// console actually bound, so an explicit --port override is honoured rather
	// than a stale default baked into the link.
	opts.Port = port

	h, closer, err := Handler(opts)
	if err != nil {
		return err
	}
	defer func() { _ = closer() }()

	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

	// A LAN-bound console ALSO listens on loopback.
	//
	// Two reasons, and the second is the load-bearing one. The operator keeps
	// local access at a stable address regardless of what the LAN listener is
	// doing; and under --pair the LAN listener can be closed and reopened
	// without taking the console down with it, which is what makes "exposure
	// lasts exactly as long as a paired device" implementable rather than
	// aspirational.
	var extra net.Listener
	if offLoopback {
		local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		if l, lerr := net.Listen("tcp", local); lerr == nil {
			extra = l
			go func() { _ = srv.Serve(l) }()
		} else {
			// Not fatal: something else may hold the loopback port. Say so
			// rather than implying local access that does not exist.
			fmt.Fprintf(out, "  note: loopback %s unavailable (%v); LAN only\n", local, lerr)
		}
	}
	defer func() {
		if extra != nil {
			_ = extra.Close()
		}
	}()

	var ln net.Listener
	if offLoopback && opts.Pairing {
		// Pairing mode: the LAN listener opens and closes with the paired
		// device set. It is NOT open at start — nothing is paired yet.
		gate, gerr := runPairGatedListener(ctx, out, srv, addr, opts.PairStorePath)
		if gerr != nil {
			return gerr
		}
		defer gate()
	} else {
		// A symbolic `lan` bind is resolved once here; only the pair-gated
		// listener above follows later network changes, because only it can
		// close and reopen a listener without taking the console down.
		target := currentPairListenerAddr(addr)
		if host, _, _ := net.SplitHostPort(target); host == BindLAN {
			return fmt.Errorf("apps: --bind %s: this host has no LAN address right now", BindLAN)
		}
		l, lerr := net.Listen("tcp", target)
		if lerr != nil {
			return fmt.Errorf("apps: listen %s: %w", target, lerr)
		}
		ln = l
		addr = target
	}

	url := "http://" + addr + "/"
	fmt.Fprintf(out, "bashy app: %s\n", url)
	for _, st := range (&probeCache{}).Probe(ctx, Discover()) {
		fmt.Fprintf(out, "  %-9s %s%s\n", st.Status, url[:len(url)-1], st.Path)
	}
	if offLoopback && opts.Pairing {
		fmt.Fprintf(out, "\n  pairing mode: the LAN listener on %s opens only while a paired\n", addr)
		fmt.Fprintf(out, "  device exists. Pair one with:  bashy app pair\n")
		fmt.Fprintf(out, "  Loopback stays up either way:  http://127.0.0.1:%d/\n", port)
	}

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if ln == nil {
		// Pairing mode owns the LAN listener; this process just waits.
		<-ctx.Done()
		return nil
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runPairGatedListener keeps the LAN listener open for exactly as long as the
// pairing state justifies it: at least one live paired device, or an
// outstanding ticket somebody is about to scan.
//
// This is the structural half of the story. Exposure is not a config flag an
// operator can leave switched on and forget — it is a FUNCTION of the paired
// device set, so when the last device is revoked or expires the port closes on
// its own. Accidental permanent exposure is not something the operator has to
// remember to avoid.
//
// The console keeps serving on loopback throughout; only the LAN listener
// comes and goes.
func runPairGatedListener(ctx context.Context, out io.Writer, srv *http.Server, addr, storePath string) (func(), error) {
	return runPairGatedListenerWithAddr(ctx, out, srv, addr, storePath, currentPairListenerAddr)
}

// runPairGatedListenerWithAddr is the testable core of
// runPairGatedListener. resolve is consulted whenever LAN access is needed so
// a long-running console follows a DHCP, Wi-Fi, or VPN address change instead
// of retrying an address the host no longer owns forever.
func runPairGatedListenerWithAddr(ctx context.Context, out io.Writer, srv *http.Server, addr, storePath string, resolve func(string) string) (func(), error) {
	if storePath == "" {
		p, err := pairPath()
		if err != nil {
			return nil, err
		}
		storePath = p
	}
	store := newPairStore(storePath)

	var mu sync.Mutex
	var ln net.Listener
	activeAddr := ""

	closeLAN := func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		if ln == nil {
			return
		}
		_ = ln.Close()
		ln = nil
		fmt.Fprintf(out, "bashy app: LAN listener on %s closed (%s)\n", activeAddr, reason)
		activeAddr = ""
	}
	openLAN := func(target, reason string) {
		mu.Lock()
		defer mu.Unlock()
		if ln != nil && activeAddr == target {
			return
		}
		l, err := net.Listen("tcp", target)
		if err != nil {
			fmt.Fprintf(out, "bashy app: could not open the LAN listener on %s: %v\n", target, err)
			return
		}
		previous := activeAddr
		if ln != nil {
			_ = ln.Close()
		}
		ln = l
		activeAddr = target
		if previous == "" {
			fmt.Fprintf(out, "bashy app: LAN listener on %s open (%s)\n", target, reason)
		} else {
			fmt.Fprintf(out, "bashy app: LAN listener moved from %s to %s (%s)\n", previous, target, reason)
		}
		go func() { _ = srv.Serve(l) }()
	}

	tick := time.NewTicker(2 * time.Second)
	done := make(chan struct{})
	go func() {
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				closeLAN("shutting down")
				return
			case <-done:
				closeLAN("shutting down")
				return
			case <-tick.C:
			}
			st, err := store.load()
			if err != nil {
				// Unreadable state closes the port. Failing closed is the only
				// safe direction for a decision about network exposure.
				closeLAN("pairing state unreadable")
				continue
			}
			now := time.Now()
			devices := len(st.liveDevices(now))
			pending := st.openTickets(now)
			switch {
			case devices > 0:
				openLAN(resolve(addr), fmt.Sprintf("%d paired device(s)", devices))
			case pending > 0:
				openLAN(resolve(addr), "a pairing code is waiting to be scanned")
			default:
				closeLAN("no paired devices")
			}
		}
	}()
	return func() { close(done) }, nil
}

// BindLAN is the symbolic --bind value meaning "this host's primary LAN
// address, whatever it is right now". It exists because a literal IP is a
// snapshot: a supervisor that guessed one at boot — before Wi-Fi was up, or on
// a secondary wired link — pinned the phone listener to an address the phone
// could never reach, and the console then preserved it faithfully because the
// host still owned it. A symbolic bind is re-resolved every time the
// pair-gated listener decides where to open, so it follows the default route
// instead of an enumeration order.
const BindLAN = "lan"

// currentPairListenerAddr preserves an address while this host still owns it.
// The symbolic BindLAN always resolves to the current primary LAN address.
// If a literal IPv4 disappeared (the ordinary DHCP, Wi-Fi, or VPN transition),
// it preserves the port and follows the host's current primary LAN address.
// Hostnames, wildcard binds, IPv6, and an unavailable address probe remain
// exactly as configured instead of being guessed at here.
func currentPairListenerAddr(configured string) string {
	host, port, err := net.SplitHostPort(configured)
	if err != nil {
		return configured
	}
	if host == BindLAN {
		if current := primaryLANAddr(); current != "" {
			return net.JoinHostPort(current, port)
		}
		return configured
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() {
		return configured
	}
	if localInterfaceHasIP(ip) {
		return configured
	}
	if current := primaryLANAddr(); current != "" {
		return net.JoinHostPort(current, port)
	}
	return configured
}

func localInterfaceHasIP(want net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch a := addr.(type) {
		case *net.IPNet:
			ip = a.IP
		case *net.IPAddr:
			ip = a.IP
		}
		if ip != nil && ip.Equal(want) {
			return true
		}
	}
	return false
}
