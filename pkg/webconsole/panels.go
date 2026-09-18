// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/webterm"
)

// Panel is one tile on the start page.
type Panel struct {
	Name  string `json:"name"`  // stable id, also the mount segment
	Label string `json:"label"` // tile title
	Path  string `json:"path"`  // app-relative path, always trailing-slashed
	Mode  string `json:"mode"`  // atlas.WebSelf | WebInProcess | WebProxy
	Port  int    `json:"port,omitempty"`

	// Start is the argv, after `bashy`, that starts a proxied service. It is the
	// answer a stopped tile shows, so the reader never has to go find the
	// command that would fix it.
	Start []string `json:"start,omitempty"`
	// StartIsFull distinguishes third-party metadata's complete argv from the
	// atlas convention, whose Start is argv after `bashy`.
	StartIsFull bool `json:"-"`

	// Icon is SVG path data on a 24 grid, or one emoji. Empty means the
	// launcher falls back to its own mark for a known name, then to the
	// initial — "a letter is honest about being a placeholder".
	Icon string `json:"icon,omitempty"`
	// Tip is the tile's title= tooltip. Empty means no tooltip.
	Tip string `json:"tip,omitempty"`

	// Auth is this panel's gate tier: public | system | custom. Empty is
	// treated as system. See consoleGate — the tier is resolved per panel, so
	// a public app opens ITS MOUNT and nothing else.
	Auth string `json:"auth,omitempty"`
	// LoginPath is the app's own sign-in path, custom tier only. Advisory: the
	// console never redirects there itself, it just does not intercept.
	LoginPath string `json:"login_path,omitempty"`

	// Source records where the declaration came from: builtin | atlas | app.
	Source string `json:"source"`

	// Available is false when the panel structurally cannot run here (no
	// pseudo-console on Windows, a handler not linked in). Distinct from
	// Status, which is about a service being up right now.
	Available bool   `json:"available"`
	Note      string `json:"note,omitempty"`

	handler http.Handler
}

// StartHint renders the complete argv as a command a human can safely copy.
func (p Panel) StartHint() string {
	if len(p.Start) == 0 {
		return ""
	}
	parts := make([]string, len(p.Start))
	for i, arg := range p.Start {
		if arg != "" && !strings.ContainsAny(arg, " \t\r\n\"'\\$`;&|<>*?()[]{}!") {
			parts[i] = arg
		} else {
			parts[i] = strconv.Quote(arg)
		}
	}
	hint := strings.Join(parts, " ")
	if !p.StartIsFull {
		hint = "bashy " + hint
	}
	return hint
}

// builtinPanels are the console's OWN panels. They are not declared in the atlas
// because no verb owns them — there is no `bashy terminal` command whose surface
// this is. Declaring them somewhere else would be indirection with one consumer.
func builtinPanels() []Panel {
	term := Panel{
		Name: "terminal", Label: "Terminal", Path: "/term/",
		Mode: atlas.WebInProcess, Source: "builtin", Available: webterm.Supported(),
		Auth: AuthSystem,
	}
	if !term.Available {
		term.Note = "this host has no pseudo-console"
	}
	files := Panel{
		Name: "files", Label: "Files", Path: "/files/",
		Mode: atlas.WebInProcess, Source: "builtin", Available: true,
		Auth: AuthSystem,
	}
	return []Panel{term, files}
}

// Discover returns the tile list: the console's own panels, plus every verb that
// declares an atlas.WebSurface. Later sources shadow earlier ones by name, which
// is the assetring precedence order.
func Discover() []Panel {
	out := map[string]Panel{}
	order := []string{}
	add := func(p Panel) {
		if _, seen := out[p.Name]; !seen {
			order = append(order, p.Name)
		}
		out[p.Name] = p
	}

	for _, p := range builtinPanels() {
		add(p)
	}

	for _, verb := range atlas.WebSurfaceNames() {
		w := atlas.WebSurfaces()[verb]
		if w.Mode == atlas.WebSelf {
			// The console does not tile itself.
			continue
		}
		add(Panel{
			Name:      verb,
			Label:     w.Label,
			Path:      "/" + w.Mount + "/",
			Mode:      w.Mode,
			Port:      w.Port,
			Start:     w.Start,
			Icon:      w.Icon,
			Tip:       w.Tip,
			Auth:      AuthSystem,
			Source:    "atlas",
			Available: true,
		})
	}

	sort.Strings(order)
	panels := make([]Panel, 0, len(order))
	for _, n := range order {
		panels = append(panels, out[n])
	}
	return panels
}

// Status is a panel plus its liveness at a moment in time.
type Status struct {
	Panel
	// Status is ready | stopped | unavailable.
	Status    string `json:"status"`
	StartHint string `json:"start_hint,omitempty"`
}

const (
	StatusReady       = "ready"
	StatusStopped     = "stopped"
	StatusUnavailable = "unavailable"
)

// probeTTL keeps a start-page repaint from re-dialling every service.
const probeTTL = 3 * time.Second

type probeCache struct {
	mu   sync.Mutex
	at   time.Time
	last []Status
}

// Probe reports each panel's liveness, dialling proxied services in parallel.
//
// A TCP connect, not an HTTP GET: several of these answer 401 or redirect at /,
// and "something is listening on the port it said it would listen on" is the
// honest signal — the same judgement pkg/meet's service status makes.
func (c *probeCache) Probe(ctx context.Context, panels []Panel) []Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < probeTTL && c.last != nil {
		return c.last
	}

	out := make([]Status, len(panels))
	var wg sync.WaitGroup
	for i, p := range panels {
		out[i] = Status{Panel: p, StartHint: p.StartHint()}
		switch {
		case !p.Available:
			out[i].Status = StatusUnavailable
		case p.Mode != atlas.WebProxy:
			out[i].Status = StatusReady
		default:
			wg.Add(1)
			go func(i int, port int) {
				defer wg.Done()
				out[i].Status = StatusStopped
				if port == 0 {
					return
				}
				d := net.Dialer{Timeout: 300 * time.Millisecond}
				conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err == nil {
					_ = conn.Close()
					out[i].Status = StatusReady
				}
			}(i, p.Port)
		}
	}
	wg.Wait()

	c.at, c.last = time.Now(), out
	return out
}

// TakenMounts returns the mount names already claimed, so a third-party app
// cannot shadow a builtin or an atlas-declared surface.
func TakenMounts(panels []Panel) map[string]bool {
	taken := map[string]bool{}
	for _, p := range panels {
		taken[p.Name] = true
		if m := strings.Trim(p.Path, "/"); m != "" {
			taken[m] = true
		}
	}
	return taken
}

// discoverApps turns --app specs into panels.
//
// It collects per-spec errors instead of aborting: one misdeclared app must not
// take the launcher down, the same judgement panelHandler already makes for an
// unmountable files panel. A spec that fails is REPORTED and dropped, never
// silently ignored — a tile that quietly vanished is indistinguishable from one
// that was never asked for.
func discoverApps(ctx context.Context, specs []string, probe ProbeFunc, taken map[string]bool, authOverrides map[string]string) ([]Panel, []error) {
	if probe == nil {
		probe = ProbeApp
	}
	var out []Panel
	var errs []error
	usedAuth := map[string]bool{}
	for _, spec := range specs {
		bin, port, err := ParseAppSpec(spec)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		meta, perr := probe(ctx, bin)
		if perr != nil {
			// The fallback rung: a binary that does not speak the contract still
			// gets a tile when the operator supplied the one fact we cannot
			// guess. Without a port there is nothing to proxy, so that IS fatal
			// for this spec.
			if !errors.Is(perr, ErrNotAnApp) {
				errs = append(errs, fmt.Errorf("%s: metadata probe failed: %w", bin, perr))
				continue
			}
			if port == 0 {
				errs = append(errs, fmt.Errorf("%s: %w, and no port given (use --app %s@<port>)", bin, perr, bin))
				continue
			}
			meta = AppMeta{}
		}
		meta.Normalize(filepath.Base(bin), port)
		if auth, ok := authOverrides[meta.Mount]; ok {
			meta.Auth = auth
			usedAuth[meta.Mount] = true
		}
		if meta.Auth != AuthCustom {
			meta.LoginPath = ""
		}
		if err := meta.Validate(taken); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", bin, err))
			continue
		}
		taken[meta.Mount] = true
		out = append(out, Panel{
			Name:        meta.Mount,
			Label:       meta.Label,
			Path:        "/" + meta.Mount + "/",
			Mode:        atlas.WebProxy,
			Port:        meta.Port,
			Start:       meta.Start,
			StartIsFull: true,
			Icon:        meta.Icon,
			Tip:         meta.Tip,
			Auth:        meta.Auth,
			LoginPath:   meta.LoginPath,
			Source:      "app",
			Available:   true,
		})
	}
	for mount := range authOverrides {
		if !usedAuth[mount] {
			errs = append(errs, fmt.Errorf("--app-auth %s has no accepted --app mount", mount))
		}
	}
	return out, errs
}

// ParseAppAuth turns explicit operator policy (`mount=tier`) into the only
// source allowed to weaken a third-party panel below system authentication.
func ParseAppAuth(values []string) (map[string]string, error) {
	out := map[string]string{}
	for _, value := range values {
		mount, auth, ok := strings.Cut(value, "=")
		mount, auth = strings.TrimSpace(mount), strings.TrimSpace(auth)
		if !ok || mount == "" || auth == "" {
			return nil, fmt.Errorf("--app-auth %q: want <mount>=public|system|custom", value)
		}
		if err := validMount(mount); err != nil {
			return nil, fmt.Errorf("--app-auth %q: %w", value, err)
		}
		switch auth {
		case AuthPublic, AuthSystem, AuthCustom:
		default:
			return nil, fmt.Errorf("--app-auth %q: unknown tier %q", value, auth)
		}
		if _, exists := out[mount]; exists {
			return nil, fmt.Errorf("--app-auth %q: mount %q is repeated", value, mount)
		}
		out[mount] = auth
	}
	return out, nil
}
