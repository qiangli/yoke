package toolcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
)

// Peer capability dispatch (Sprint 418 S9). `tool cmd run --capability X
// --peer NAME` sends the capability request to NAME's model door — normally
// reached through the outpost mesh forwarder (`outpost mesh dial llm`) — as an
// ordinary chat completion carrying the capability header. The PEER's door
// selects the provider from its own host measurements and quota and runs the
// declared command through its slash path (pkg/cligw capability.go); the door
// admits the request only for a per-peer credential the peer's owner minted
// and shared (`llm peer add|share`, pkg/broker peershare.go). No new plane: it
// is the door's existing inference route.
//
// The caller's side of that contract is a local 0600 file naming each peer's
// door URL and credential:
//
//	$BASHY_HOME/toolcmd/peers.json  (else ~/.bashy/toolcmd/peers.json)
//	{"peers": {"dragon": {"url": "http://127.0.0.1:24600", "credential": "bpeer_..."}}}
//
// An unknown peer, an unreadable or group/other-readable file, or a missing
// credential fails closed before any network call. Neither the credential nor
// the URL is ever echoed.

// Headers of a capability request; pkg/cligw serves them.
const (
	CapabilityHeader = "X-Bashy-Capability"
	CallerHeader     = "X-Bashy-Caller"
)

// DefaultPeerTimeout bounds a peer capability run when --timeout is unset: a
// deep-research command legitimately runs for many minutes.
const DefaultPeerTimeout = 30 * time.Minute

// ErrUnknownPeer refuses a --peer that the local peers file does not name.
var ErrUnknownPeer = errors.New("toolcmd: unknown peer")

var peerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// PeerDoor is one configured peer door.
type PeerDoor struct {
	URL        string `json:"url"`
	Credential string `json:"credential"`
}

type peersFile struct {
	Peers map[string]PeerDoor `json:"peers"`
}

// PeersPath is the caller's peer door config.
func PeersPath() (string, error) {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "toolcmd", "peers.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("toolcmd: resolve bashy home: %w", err)
	}
	return filepath.Join(home, ".bashy", "toolcmd", "peers.json"), nil
}

// LoadPeer resolves name from the peers file, refusing anything ambiguous.
func LoadPeer(name string) (PeerDoor, error) {
	name = strings.TrimSpace(name)
	if !peerNameRE.MatchString(name) {
		// Deliberately not echoed: the value may be a URL carrying a secret.
		return PeerDoor{}, fmt.Errorf("%w: --peer must be a peer name configured in the toolcmd peers file ([a-z0-9][a-z0-9._-]*), not an address", ErrUnknownPeer)
	}
	path, err := PeersPath()
	if err != nil {
		return PeerDoor{}, err
	}
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PeerDoor{}, fmt.Errorf("%w %q: no peers file at %s", ErrUnknownPeer, name, path)
	}
	if err != nil {
		return PeerDoor{}, fmt.Errorf("toolcmd: peers file: %w", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return PeerDoor{}, fmt.Errorf("toolcmd: peers file %s is mode %04o; it holds credentials and must be 0600", path, fi.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return PeerDoor{}, fmt.Errorf("toolcmd: peers file: %w", err)
	}
	var pf peersFile
	if err := json.Unmarshal(data, &pf); err != nil {
		return PeerDoor{}, fmt.Errorf("toolcmd: peers file %s is not valid JSON", path)
	}
	p, ok := pf.Peers[name]
	if !ok {
		return PeerDoor{}, fmt.Errorf("%w %q: not in %s", ErrUnknownPeer, name, path)
	}
	u, err := url.Parse(strings.TrimSpace(p.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return PeerDoor{}, fmt.Errorf("toolcmd: peer %q has no valid http(s) door url (credentials belong in \"credential\", never the url)", name)
	}
	if strings.TrimSpace(p.Credential) == "" {
		return PeerDoor{}, fmt.Errorf("toolcmd: peer %q has no credential (the peer owner mints one with `bashy llm peer add`)", name)
	}
	p.URL, p.Credential = strings.TrimRight(u.String(), "/"), strings.TrimSpace(p.Credential)
	return p, nil
}

// peerHTTPClient never follows redirects: the credential is for one door.
var peerHTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// DispatchPeerCapability runs capText on peer name's door and returns the
// peer's typed result. The selection event is recorded on THIS host, naming
// the peer, the agent it selected and the vendor that served the command.
func DispatchPeerCapability(ctx context.Context, name, capText, caller, text string, timeout time.Duration) (Result, error) {
	c, ok := capability.ParseCapability(capText)
	if !ok {
		return Result{}, fmt.Errorf("toolcmd: unknown capability %q", capText)
	}
	door, err := LoadPeer(name)
	if err != nil {
		return Result{}, err
	}
	name = strings.TrimSpace(name)
	callerVendor := callerTool(caller)
	scrub := func(s string) string { return strings.ReplaceAll(s, door.Credential, "[redacted]") }

	body, _ := json.Marshal(map[string]any{
		"model":    "auto",
		"messages": []map[string]string{{"role": "user", "content": text}},
	})
	if timeout <= 0 {
		timeout = DefaultPeerTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, door.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("toolcmd: peer %q request: %s", name, scrub(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+door.Credential)
	req.Header.Set(CapabilityHeader, string(c))
	if callerVendor != "" {
		req.Header.Set(CallerHeader, callerVendor)
	}
	resp, err := peerHTTPClient.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // the url is not echoed
		}
		return Result{}, fmt.Errorf("toolcmd: peer %q door unreachable: %s", name, scrub(err.Error()))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return Result{}, fmt.Errorf("toolcmd: peer %q read reply: %s", name, scrub(err.Error()))
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("toolcmd: peer %q refused capability %s (HTTP %d): %s", name, c, resp.StatusCode, scrub(peerErrorText(data)))
	}
	var reply struct {
		ToolCommand *Result `json:"x_bashy_tool_command"`
	}
	if err := json.Unmarshal(data, &reply); err != nil || reply.ToolCommand == nil {
		return Result{}, fmt.Errorf("toolcmd: peer %q reply carries no tool-command envelope", name)
	}
	res := *reply.ToolCommand
	sel := res.Selection
	if sel == nil || sel.Vendor == "" || sel.Binding == "" {
		return res, fmt.Errorf("toolcmd: peer %q reply carries no measured selection; refusing an unattributed report", name)
	}
	if sel.Capability != string(c) {
		return res, fmt.Errorf("toolcmd: peer %q answered capability %q, asked %q", name, sel.Capability, c)
	}
	if callerVendor != "" && sel.Vendor == callerVendor {
		return res, fmt.Errorf("toolcmd: peer %q selected the caller's own vendor %s", name, sel.Vendor)
	}
	if res.Tool != "" && res.Tool != sel.Vendor {
		return res, fmt.Errorf("toolcmd: peer %q selection names %s but %s served the command", name, sel.Vendor, res.Tool)
	}
	sel.Peer = name
	if err := recordSelection(*sel); err != nil {
		return res, fmt.Errorf("toolcmd: publish capability selection: %w", err)
	}
	return res, nil
}

func peerErrorText(data []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil && len(env.Error) > 0 {
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
		var s string
		if json.Unmarshal(env.Error, &s) == nil && s != "" {
			return s
		}
	}
	t := strings.TrimSpace(string(data))
	if len(t) > 400 {
		t = t[:400] + "..."
	}
	return t
}
