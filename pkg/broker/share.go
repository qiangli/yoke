package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// Sharing the door with an owned peer. Two commands, no token to carry:
//
//	door$  bashy llm share PEER [--via mesh|cloudbox|both]
//	peer$  eval "$(bashy llm env --from DOOR [--via mesh|cloudbox])"
//
// share exposes the door (a persistent outpost mesh service and/or the
// door's cloudbox satellite app), mints PEER's credential with share=allow,
// and PUSHES a grant to PEER over the owned `outpost ssh` channel into
// `bashy llm door import` — on stdin, never argv, never printed here. env
// --from reads that grant, keeps a persistent mesh forward to the door, and
// prints the usual client blocks keyed by the peer credential, never the
// owner token. Vendor logins stay on the door host.

// GrantSchemaVersion tags the grant a door pushes to a peer.
const GrantSchemaVersion = "bashy-llm-door-grant-v1"

// doorServiceName names the door's mesh service and its cloudbox app.
const doorServiceName = "llm"

// DoorGrant is what a peer holds for one shared door.
type DoorGrant struct {
	Schema string `json:"schema_version"`
	// Door is the door host's outpost name; the peer addresses it by it.
	Door string `json:"door"`
	// Peer is the principal the credential authenticates as (peer:NAME).
	Peer        string `json:"peer"`
	MeshPeerID  string `json:"mesh_peer_id,omitempty"`
	MeshService string `json:"mesh_service,omitempty"`
	// CloudboxURL is the door's satellite app base URL, without a key.
	CloudboxURL string    `json:"cloudbox_url,omitempty"`
	Credential  string    `json:"credential"`
	Issued      time.Time `json:"issued"`
}

func (g DoorGrant) validate() error {
	switch {
	case g.Schema != GrantSchemaVersion:
		return fmt.Errorf("unsupported grant schema %q (want %s)", g.Schema, GrantSchemaVersion)
	case !ValidPeerName(strings.ToLower(g.Door)):
		return errors.New("grant has no valid door host name")
	case !ValidPeerName(g.Peer):
		return errors.New("grant has no valid peer name")
	case !strings.HasPrefix(g.Credential, "bpeer_"):
		return errors.New("grant carries no peer credential")
	case (g.MeshPeerID == "") != (g.MeshService == ""):
		return errors.New("grant names half a mesh route")
	case g.MeshPeerID == "" && g.CloudboxURL == "":
		return errors.New("grant names no route (mesh or cloudbox)")
	}
	if g.CloudboxURL != "" {
		u, err := url.Parse(g.CloudboxURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return errors.New("grant has an invalid cloudbox url (want https)")
		}
	}
	return nil
}

// outpostRun runs the outpost CLI and returns its stdout (a seam for tests).
// Arguments never carry a credential; stdin may.
var outpostRun = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "outpost", args...)
	c.Stdin = stdin
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		verb := strings.Join(args[:min(len(args), 3)], " ")
		return out, fmt.Errorf("outpost %s: %v: %s", verb, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// outpostOnPath reports whether a Linux-side outpost exists (a seam for tests).
var outpostOnPath = func() bool {
	_, err := exec.LookPath("outpost")
	return err == nil
}

// outpostWinRun runs the Windows outpost through WSL interop, for a WSL distro
// that holds its own grant but no outpost: the forward lives on the Windows
// side, bound on the address WSL reaches Windows at (a seam for tests).
var outpostWinRun = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "outpost.exe", args...)
	c.Stdin = stdin
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("outpost.exe %s: %v: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ---- door host ---------------------------------------------------------------

// outpostSelf reads this host's outpost name and cloudbox URL.
func outpostSelf(ctx context.Context) (name, cloudbox string, err error) {
	out, err := outpostRun(ctx, nil, "status", "--json")
	if err != nil {
		return "", "", err
	}
	var st struct {
		Config struct {
			AgentName   string `json:"agent_name"`
			CloudboxURL string `json:"cloudbox_url"`
		} `json:"config"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", "", fmt.Errorf("outpost status: %w", err)
	}
	if st.Config.AgentName == "" {
		return "", "", errors.New("outpost status names no host (is this host paired? `outpost connect`)")
	}
	return st.Config.AgentName, strings.TrimRight(st.Config.CloudboxURL, "/"), nil
}

// ensureMeshService exposes addr over the mesh unless a persistent service
// already does, and returns the service name and this host's mesh peer id.
func ensureMeshService(ctx context.Context, addr string) (service, peerID string, err error) {
	out, err := outpostRun(ctx, nil, "mesh", "service", "ls")
	if err != nil {
		return "", "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		name, target, ok := strings.Cut(strings.TrimSpace(line), " -> ")
		if ok && strings.TrimSpace(target) == addr {
			service = strings.TrimSpace(name)
			break
		}
	}
	if service == "" {
		service = doorServiceName
		if _, err := outpostRun(ctx, nil, "mesh", "service", "add", service, addr); err != nil {
			return "", "", err
		}
	}
	out, err = outpostRun(ctx, nil, "mesh", "status", "--json")
	if err != nil {
		return "", "", err
	}
	var st struct {
		Status struct {
			PeerID string `json:"peer_id"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &st); err != nil || st.Status.PeerID == "" {
		return "", "", errors.New("outpost mesh status reports no peer id (is the mesh enabled?)")
	}
	return service, st.Status.PeerID, nil
}

// ensureCloudboxApp registers the door as this host's public satellite app
// unless it already is, and returns its base URL. The door authenticates
// every request itself; the cloudbox front strips Authorization, so a peer
// keys its requests in the path (/k/<credential>).
func ensureCloudboxApp(ctx context.Context, host, cloudbox string, port int) (string, error) {
	if cloudbox == "" {
		return "", errors.New("this host has no cloudbox (outpost status names none)")
	}
	out, err := outpostRun(ctx, nil, "apps", "list", "--json")
	if err != nil {
		return "", err
	}
	var list struct {
		Apps []struct {
			Name         string `json:"name"`
			Host         string `json:"host"`
			Port         int    `json:"port"`
			Enabled      bool   `json:"enabled"`
			RequireLogin bool   `json:"require_login"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("outpost apps list: %w", err)
	}
	// Name+port+enabled+!login is not enough: an app wired by hand before
	// share existed can carry the port inside its host ("127.0.0.1:17788"
	// plus the door's port), which cloudbox proxies to a 502. Only a bare
	// loopback host counts as present; anything else is re-added below.
	present := false
	for _, a := range list.Apps {
		if a.Name == doorServiceName && a.Port == port && a.Enabled && !a.RequireLogin && bareLoopbackHost(a.Host) {
			present = true
		}
	}
	if !present {
		if _, err := outpostRun(ctx, nil, "apps", "add", doorServiceName, "--url", fmt.Sprintf("http://127.0.0.1:%d", port)); err != nil {
			return "", err
		}
	}
	return cloudbox + "/matrix/h/" + url.PathEscape(host) + "/app/" + doorServiceName, nil
}

// olderPeerBashy reports whether err is the peer's bashy refusing the
// `llm door import` subcommand — cobra's unknown-command text carried back
// over outpost ssh. That is version skew, not an unreachable host, so share
// names the fix instead of surfacing the raw usage error.
func olderPeerBashy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, want := range []string{
		// cobra names the command path with or without the "bashy " root.
		`unknown command "import" for "`,
		`unknown command "door" for "`,
	} {
		if strings.Contains(msg, want) {
			return true
		}
	}
	return false
}

// bareLoopbackHost reports whether an app entry's host is a bare
// loopback name — 127.0.0.1, localhost or ::1 — with no port embedded in
// it. An app whose host carries a port ("127.0.0.1:17788") points at the
// wrong upstream and must be re-added.
func bareLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if _, _, err := net.SplitHostPort(h); err == nil {
		return false
	}
	for _, loop := range []string{"127.0.0.1", "localhost", "::1", "[::1]"} {
		if strings.EqualFold(h, loop) {
			return true
		}
	}
	return false
}

func newShareCmd() *cobra.Command {
	var via, sshTarget string
	var printGrant bool
	cmd := &cobra.Command{
		Use:   "share PEER",
		Short: "share this door with an owned peer host (one command; the peer then runs `llm env --from`)",
		Long: `share makes this door usable from PEER, an owned host, in one step:

  mesh      a persistent outpost mesh service for the door (reused if present)
  cloudbox  the door as this host's public satellite app "llm"; the door still
            authenticates every request, keyed in the path (/k/<credential>)

It mints (or rotates) PEER's credential with share allow and pushes it to PEER
over the owned ` + "`outpost ssh`" + ` channel into ` + "`bashy llm door import`" + ` (on stdin; the
credential is never printed or logged here). On PEER, then:

  eval "$(bashy llm env --from THIS-HOST)"

Rerunning share is safe: it reuses the exposure and rotates the credential.
--ssh names the outpost ssh target when it is not PEER (user@host). --print
writes the grant to stdout instead of pushing it — only for a peer this host
cannot reach; pipe it to ` + "`bashy llm door import`" + ` there and keep it secret.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			peer := strings.ToLower(strings.TrimSpace(args[0]))
			if !ValidPeerName(peer) {
				return fmt.Errorf("llm share: invalid peer name %q (want [a-z0-9][a-z0-9._-]*)", args[0])
			}
			via = strings.ToLower(strings.TrimSpace(via))
			if via != "mesh" && via != "cloudbox" && via != "both" {
				return fmt.Errorf("llm share: unknown route %q (want mesh, cloudbox or both)", via)
			}
			ctx := c.Context()
			host, cloudbox, err := outpostSelf(ctx)
			if err != nil {
				return fmt.Errorf("llm share: %w", err)
			}
			g := DoorGrant{Schema: GrantSchemaVersion, Door: strings.ToLower(host), Peer: peer, Issued: time.Now().UTC()}
			if via != "cloudbox" {
				if g.MeshService, g.MeshPeerID, err = ensureMeshService(ctx, fmt.Sprintf("127.0.0.1:%d", door.Port())); err != nil {
					return fmt.Errorf("llm share: mesh: %w", err)
				}
			}
			if via != "mesh" {
				if g.CloudboxURL, err = ensureCloudboxApp(ctx, host, cloudbox, door.Port()); err != nil {
					return fmt.Errorf("llm share: cloudbox: %w", err)
				}
			}
			path, err := peersPath()
			if err != nil {
				return err
			}
			if g.Credential, err = MintPeerCredential(path, peer, ShareAllow, g.Issued); err != nil {
				return err
			}
			data, err := json.Marshal(g)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if printGrant {
				_, err := fmt.Fprintf(out, "%s\n", data)
				return err
			}
			target := firstNonEmpty(strings.TrimSpace(sshTarget), peer)
			if _, err := outpostRun(ctx, bytes.NewReader(data), "ssh", target, "bashy", "llm", "door", "import"); err != nil {
				if olderPeerBashy(err) {
					return fmt.Errorf("llm share: the bashy on %s is older than this door; upgrade it (or --print | <candidate> llm door import)", peer)
				}
				return fmt.Errorf("llm share: push the credential to %s: %w (rerun when it is reachable, or see --print)", target, err)
			}
			fmt.Fprintf(out, "shared this door with peer:%s via %s\non %s: eval \"$(bashy llm env --from %s)\"\n", peer, via, peer, g.Door)
			return nil
		},
	}
	cmd.Flags().StringVar(&via, "via", "mesh", "expose the door over mesh|cloudbox|both")
	cmd.Flags().StringVar(&sshTarget, "ssh", "", "outpost ssh target when it is not PEER (e.g. user@host)")
	cmd.Flags().BoolVar(&printGrant, "print", false, "print the grant (it holds the credential) instead of pushing it to PEER")
	return cmd
}

// ---- peer --------------------------------------------------------------------

func doorGrantPath(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !ValidPeerName(name) {
		return "", fmt.Errorf("invalid door host name %q", name)
	}
	dir, err := door.DoorsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// SaveDoorGrant stores g owner-only (0600 in a 0700 directory; an
// owner-only ACL on Windows).
func SaveDoorGrant(g DoorGrant) error {
	if err := g.validate(); err != nil {
		return err
	}
	path, err := doorGrantPath(g.Door)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := secureDir(dir); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadDoorGrant reads the grant for door host name; a missing one wraps
// os.ErrNotExist.
func LoadDoorGrant(name string) (DoorGrant, error) {
	path, err := doorGrantPath(name)
	if err != nil {
		return DoorGrant{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DoorGrant{}, err
	}
	var g DoorGrant
	if err := json.Unmarshal(data, &g); err != nil {
		return DoorGrant{}, fmt.Errorf("door grant %s: %w", path, err)
	}
	if err := g.validate(); err != nil {
		return DoorGrant{}, fmt.Errorf("door grant %s: %w", path, err)
	}
	return g, nil
}

func newDoorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "door",
		Short: "doors other hosts shared with this one (`llm share` on the door host)",
	}
	imp := &cobra.Command{
		Use:   "import",
		Short: "store a door grant read from stdin (run by `llm share` over outpost ssh)",
		Long: `import reads a ` + GrantSchemaVersion + ` grant on stdin and stores it owner-only
under $BASHY_HOME/llm/doors/<door>.json. It also opens the persistent mesh
forward and records the door in the toolcmd peers file, so ` + "`tool cmd run --peer`" + `
reaches it. The grant is also a live model source: ` + "`genie -m MODEL@DOOR`" + `
(or DOOR-door-MODEL) reads its current credential each invocation, including
after a share rotation; no model YAML or secrets.map is needed.
` + "`llm share`" + ` on the door host runs this for you.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			data, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 64<<10))
			if err != nil {
				return err
			}
			var g DoorGrant
			if err := json.Unmarshal(data, &g); err != nil {
				return fmt.Errorf("llm door import: stdin is not a door grant: %w", err)
			}
			g.Door = strings.ToLower(g.Door)
			if err := SaveDoorGrant(g); err != nil {
				return fmt.Errorf("llm door import: %w", err)
			}
			out := c.OutOrStdout()
			if pd, err := grantPeerDoor(c.Context(), g); err != nil {
				fmt.Fprintf(c.ErrOrStderr(), "llm door import: toolcmd peer %s not recorded: %v\n", g.Door, err)
			} else if err := toolcmd.SavePeer(g.Door, pd); err != nil {
				fmt.Fprintf(c.ErrOrStderr(), "llm door import: toolcmd peer %s not recorded: %v\n", g.Door, err)
			}
			fmt.Fprintf(out, "imported door %s as peer:%s; eval \"$(bashy llm env --from %s)\"\n", g.Door, g.Peer, g.Door)
			fmt.Fprintf(out, "genie model source: bashy genie -m MODEL@%s PROMPT (also %s-door-MODEL); credentials follow share rotations\n", g.Door, g.Door)
			return nil
		},
	}
	cmd.AddCommand(imp)
	return cmd
}

// grantPeerDoor is the toolcmd peer entry for g: the mesh forward when the
// door shares over the mesh, else its cloudbox app keyed in the path.
func grantPeerDoor(ctx context.Context, g DoorGrant) (toolcmd.PeerDoor, error) {
	if g.MeshPeerID != "" {
		addr, err := ensureMeshForward(ctx, g, meshBindHost())
		if err == nil || g.CloudboxURL == "" {
			return toolcmd.PeerDoor{URL: "http://" + addr, Credential: g.Credential}, err
		}
	}
	return toolcmd.PeerDoor{URL: g.CloudboxURL + "/k/" + g.Credential, Credential: g.Credential}, nil
}

// meshBindHost is where a new mesh forward listens by default: loopback,
// except in a WSL distro with no outpost of its own, where the forward lives
// in the Windows outpost and must listen on the address WSL reaches Windows
// at (a Windows loopback port is unreachable from WSL).
func meshBindHost() string {
	if inWSL() && !outpostOnPath() {
		if gw, err := wslGateway(); err == nil && gw != "" {
			return gw
		}
	}
	return "127.0.0.1"
}

// ensureMeshForward returns the local address of a persistent mesh forward to
// g's door bound on bindHost, opening one on a free port when none exists.
func ensureMeshForward(ctx context.Context, g DoorGrant, bindHost string) (string, error) {
	run, winPort := outpostRun, false
	if inWSL() && !outpostOnPath() {
		run, winPort = outpostWinRun, true
	}
	out, err := run(ctx, nil, "mesh", "consume", "ls")
	if err != nil {
		return "", err
	}
	// Lines read "SERVICE <- peer PEERID -> ADDR".
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 7 && f[6] != "[live]" {
			continue
		}
		if (len(f) == 6 || len(f) == 7) && f[0] == g.MeshService && f[3] == g.MeshPeerID {
			if h, p, err := net.SplitHostPort(f[5]); err == nil && h == bindHost {
				if p == "0" {
					// A persisted ephemeral request is not a real address.
					// outpost's syntax is `consume rm SERVICE --local ADDR`:
					// remove exactly this entry, not every consume of the service.
					_, _ = run(ctx, nil, "mesh", "consume", "rm", g.MeshService, "--local", f[5])
					continue
				}
				return f[5], nil
			}
		}
	}
	if winPort {
		// WSL cannot probe a port on the Windows side, and outpost persists
		// the requested address verbatim, so request a concrete one.
		h := fnv.New32a()
		h.Write([]byte(g.MeshService + "\x00" + g.MeshPeerID))
		base := int(h.Sum32() % 16000)
		var lastErr error
		for i := 0; i < 16; i++ {
			addr := net.JoinHostPort(bindHost, strconv.Itoa(48000+(base+i)%16000))
			out, err := run(ctx, nil, "mesh", "consume", "add", g.MeshService, g.MeshPeerID, "--local", addr)
			if err != nil {
				lastErr = err
				if strings.Contains(strings.ToLower(err.Error()), "address already in use") || strings.Contains(strings.ToLower(err.Error()), "address in use") {
					continue
				}
				return "", err
			}
			return meshAddrFrom(out, addr), nil
		}
		return "", lastErr
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bindHost, "0"))
	if err != nil {
		return "", fmt.Errorf("pick a local port on %s: %w", bindHost, err)
	}
	addr := ln.Addr().String()
	ln.Close()
	out, err = run(ctx, nil, "mesh", "consume", "add", g.MeshService, g.MeshPeerID, "--local", addr)
	if err != nil {
		return "", err
	}
	return meshAddrFrom(out, addr), nil
}

func meshAddrFrom(out []byte, addr string) string {
	if _, got, ok := strings.Cut(strings.TrimSpace(string(out)), " -> "); ok {
		if _, p, err := net.SplitHostPort(got); err == nil && p != "0" {
			return got
		}
	}
	return addr
}

// peerEnvVars is the client environment for a peer: base already carries
// the key in the path when keyInPath (the cloudbox front strips
// Authorization); the *_API_KEY values are the credential either way.
func peerEnvVars(base, cred string, keyInPath bool) map[string]string {
	ollama := base + "/k/" + cred
	if keyInPath {
		ollama = base
	}
	return map[string]string{
		"OPENAI_BASE_URL":    base + "/v1",
		"OPENAI_API_KEY":     cred,
		"ANTHROPIC_BASE_URL": base + "/anthropic",
		"ANTHROPIC_API_KEY":  cred,
		"OLLAMA_HOST":        ollama,
	}
}

// envFrom resolves `llm env --from DOOR`: the base URL, the client env and the
// route taken. via "" picks mesh when the grant has it, else cloudbox.
func envFrom(ctx context.Context, from, via, bindHost string) (string, map[string]string, string, error) {
	g, err := LoadDoorGrant(from)
	if errors.Is(err, os.ErrNotExist) {
		if inWSL() {
			return wslEnvFrom(ctx, from, via)
		}
		return "", nil, "", fmt.Errorf("llm env: no door grant from %q on this host; run `bashy llm share %s` on %s", from, thisHostName(), from)
	}
	if err != nil {
		return "", nil, "", fmt.Errorf("llm env: %w", err)
	}
	if via == "" {
		via = "cloudbox"
		if g.MeshPeerID != "" {
			via = "mesh"
		}
	}
	switch via {
	case "mesh":
		if g.MeshPeerID == "" {
			return "", nil, "", fmt.Errorf("llm env: %s did not share its door over the mesh; use --via cloudbox, or run `bashy llm share %s --via both` on %s", g.Door, thisHostName(), g.Door)
		}
		if bindHost == "" {
			bindHost = meshBindHost()
		}
		addr, err := ensureMeshForward(ctx, g, bindHost)
		if err != nil {
			return "", nil, "", fmt.Errorf("llm env: mesh forward to %s: %w", g.Door, err)
		}
		base := "http://" + addr
		return base, peerEnvVars(base, g.Credential, false), via, nil
	case "cloudbox":
		if g.CloudboxURL == "" {
			return "", nil, "", fmt.Errorf("llm env: %s did not share its door over cloudbox; run `bashy llm share %s --via both` on %s", g.Door, thisHostName(), g.Door)
		}
		base := g.CloudboxURL + "/k/" + g.Credential
		return g.CloudboxURL, peerEnvVars(base, g.Credential, true), via, nil
	}
	return "", nil, "", fmt.Errorf("llm env: unknown route %q with --from (want mesh or cloudbox)", via)
}

// thisHostName is the short host name, as a door host would address it.
func thisHostName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "THIS-HOST"
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// ---- WSL ---------------------------------------------------------------------

// inWSL reports whether this process runs inside WSL (a seam for tests).
var inWSL = func() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return bytes.Contains(bytes.ToLower(b), []byte("microsoft"))
}

// wslInterop runs the Windows bashy through WSL interop (a seam for tests).
var wslInterop = func(ctx context.Context, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "bashy.exe", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, errors.New(strings.TrimSpace(stderr.String()))
		}
		return out, errWSLInterop
	}
	return out, nil
}

var errWSLInterop = errors.New("WSL interop is unavailable (bashy.exe cannot run from here)")

// wslGateway is the Windows host's address as WSL sees it (a seam for tests).
var wslGateway = func() (string, error) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", err
	}
	return defaultGateway(string(data))
}

// defaultGateway reads the default route's gateway from /proc/net/route.
func defaultGateway(table string) (string, error) {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		// The kernel prints the address in host (little-endian) order.
		return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)).String(), nil
	}
	return "", errors.New("no default route")
}

// wslEnvFrom serves a WSL distro that holds no grant of its own from the
// Windows user's: the Windows bashy reads its store and, for mesh, its
// outpost owns a forward bound on the address WSL reaches Windows at.
func wslEnvFrom(ctx context.Context, from, via string) (string, map[string]string, string, error) {
	args := []string{"llm", "env", "--from", from, "--json"}
	if via != "" {
		args = append(args, "--via", via)
	}
	if via != "cloudbox" {
		gw, err := wslGateway()
		if err != nil {
			return "", nil, "", fmt.Errorf("llm env: WSL: find the Windows host address: %w (or use --via cloudbox)", err)
		}
		args = append(args, "--bind-for-wsl", gw)
	}
	out, err := wslInterop(ctx, args...)
	if errors.Is(err, errWSLInterop) {
		return "", nil, "", fmt.Errorf("llm env: no door grant from %q in this WSL distro and %v; enable interop, or share the door with this distro directly and use --via cloudbox", from, err)
	}
	if err != nil {
		return "", nil, "", fmt.Errorf("llm env: via Windows: %w", err)
	}
	var env struct {
		BaseURL string            `json:"base_url"`
		Via     string            `json:"via"`
		Env     map[string]string `json:"env"`
	}
	if err := json.Unmarshal(out, &env); err != nil || env.Env["OPENAI_BASE_URL"] == "" {
		return "", nil, "", errors.New("llm env: the Windows bashy returned no client environment")
	}
	return env.BaseURL, env.Env, env.Via, nil
}
