package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

func init() {
	RegisterProvider("peer", peerProvider)
}

// peerTimeout bounds every outpost invocation; peerMaxOutput caps its body.
const peerTimeout = 15 * time.Second

const peerMaxOutput = 4 << 20

// peerRunner runs the single read-only outpost query behind the peer: scheme.
// It is a variable so tests can inject canned JSON without a live peer.
var peerRunner = runOutpostPeersStatus

// peerCapture drains stdout while retaining at most peerMaxOutput bytes.
// Returning the full write length prevents a large producer from blocking on
// its stdout pipe while keeping the snapshot's memory bound.
type peerCapture struct {
	bytes.Buffer
	overflow bool
}

func (w *peerCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := peerMaxOutput - w.Len()
	if n > remaining {
		w.overflow = true
		p = p[:remaining]
	}
	_, _ = w.Buffer.Write(p)
	return n, nil
}

// runOutpostPeersStatus invokes exactly one read-only command:
// outpost peers status --json. No write flags are ever passed.
func runOutpostPeersStatus(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, peerTimeout)
	defer cancel()
	var output peerCapture
	cmd := exec.CommandContext(ctx, "outpost", "peers", "status", "--json")
	cmd.Stdout = &output
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	if output.overflow {
		return nil, fmt.Errorf("peer: outpost peers status output exceeds %d bytes", peerMaxOutput)
	}
	return output.Bytes(), nil
}

// peerStatus is one row of `outpost peers status --json`.
type peerStatus struct {
	Host       string `json:"host"`
	Alias      string `json:"alias,omitempty"`
	Owned      bool   `json:"owned"`
	Shared     bool   `json:"shared,omitempty"`
	Online     bool   `json:"online"`
	Location   string `json:"location,omitempty"`
	Version    string `json:"version,omitempty"`
	Commit     string `json:"commit,omitempty"`
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	OSVersion  string `json:"os_version,omitempty"`
	UpdateMode string `json:"update_mode,omitempty"`
}

// fetchPeers runs the bounded outpost query and returns peers sorted by host.
// Rows without a host are dropped; the query output is never executed.
func fetchPeers(ctx context.Context) ([]peerStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := peerRunner(ctx)
	if err != nil {
		return nil, err
	}
	var decoded []peerStatus
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("peer: decode outpost peers status: %w", err)
	}
	peers := make([]peerStatus, 0, len(decoded))
	for _, p := range decoded {
		if p.Host == "" {
			continue
		}
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Host < peers[j].Host })
	return peers, nil
}

func peerRootNode(count uint64) provider.Node {
	return provider.Node{
		SchemaVersion: provider.NodeSchema,
		Ref:           "peer:",
		Path:          "",
		Name:          "peer",
		Kind:          "directory",
		Type:          "peer/tree",
		Children:      &count,
		Attrs:         map[string]any{},
	}
}

func peerNode(p peerStatus) provider.Node {
	attrs := map[string]any{
		"host":   p.Host,
		"owned":  p.Owned,
		"online": p.Online,
	}
	if p.Alias != "" {
		attrs["alias"] = p.Alias
	}
	if p.Shared {
		attrs["shared"] = true
	}
	if p.Location != "" {
		attrs["location"] = p.Location
	}
	if p.Version != "" {
		attrs["version"] = p.Version
	}
	if p.Commit != "" {
		attrs["commit"] = p.Commit
	}
	if p.OS != "" {
		attrs["os"] = p.OS
	}
	if p.Arch != "" {
		attrs["arch"] = p.Arch
	}
	if p.OSVersion != "" {
		attrs["os_version"] = p.OSVersion
	}
	if p.UpdateMode != "" {
		attrs["update_mode"] = p.UpdateMode
	}
	return provider.Node{
		SchemaVersion: provider.NodeSchema,
		Ref:           ref.Locator{Scheme: "peer", Target: p.Host}.String(),
		Path:          p.Host,
		Name:          p.Host,
		Kind:          "peer",
		Type:          "peer/host",
		Attrs:         attrs,
	}
}

// peerSnapshot resolves loc against one bounded outpost query. It reports
// whether loc names the root and, otherwise, the matching peer (by host,
// falling back to alias).
func peerSnapshot(ctx context.Context, loc ref.Locator) (peers []peerStatus, isRoot bool, current peerStatus, err error) {
	if loc.Scheme != "peer" || len(loc.Inner) != 0 || loc.ScopeKind != "" {
		return nil, false, peerStatus{}, provider.ErrUnsupported
	}
	peers, err = fetchPeers(ctx)
	if err != nil {
		return nil, false, peerStatus{}, err
	}
	t := strings.TrimSuffix(strings.TrimPrefix(loc.Target, "/"), "/")
	if t == "" || t == "." {
		return peers, true, peerStatus{}, nil
	}
	for _, p := range peers {
		if p.Host == t || (p.Alias != "" && p.Alias == t) {
			return peers, false, p, nil
		}
	}
	return nil, false, peerStatus{}, fmt.Errorf("provider: peer %q: %w", t, os.ErrNotExist)
}

// peerRef is the canonical ref of loc: the root spells "peer:", a peer
// spells "peer:<host>" even when addressed by alias.
func peerRef(isRoot bool, current peerStatus) string {
	if isRoot {
		return "peer:"
	}
	return peerNode(current).Ref
}

func encodeNodes(nodes []provider.Node) (io.ReadCloser, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, n := range nodes {
		if err := enc.Encode(n); err != nil {
			return nil, err
		}
	}
	return io.NopCloser(&b), nil
}

// peerRecipe serves the outpost status board as canonical node/edge rows.
// List and Edges enumerate the board; Stat addresses the root or one peer.
func peerRecipe() provider.Recipe {
	return provider.Recipe{
		ListJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			peers, isRoot, _, err := peerSnapshot(ctx, loc)
			if err != nil {
				return nil, err
			}
			if !isRoot {
				return io.NopCloser(strings.NewReader("")), nil
			}
			nodes := make([]provider.Node, 0, len(peers))
			for _, p := range peers {
				nodes = append(nodes, peerNode(p))
			}
			return encodeNodes(nodes)
		},
		StatNode: func(ctx context.Context, loc ref.Locator) (provider.Node, error) {
			peers, isRoot, current, err := peerSnapshot(ctx, loc)
			if err != nil {
				return provider.Node{}, err
			}
			if isRoot {
				count := uint64(len(peers))
				return peerRootNode(count), nil
			}
			return peerNode(current), nil
		},
		EdgesJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			peers, isRoot, _, err := peerSnapshot(ctx, loc)
			if err != nil {
				return nil, err
			}
			if !isRoot {
				return io.NopCloser(strings.NewReader("")), nil
			}
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			for _, p := range peers {
				if err := enc.Encode(provider.Edge{
					SchemaVersion: provider.EdgeSchema,
					Src:           peerRef(true, peerStatus{}),
					Rel:           "contains",
					Dst:           peerNode(p).Ref,
					Attrs:         map[string]any{},
				}); err != nil {
					return nil, err
				}
			}
			return io.NopCloser(&b), nil
		},
	}
}

func peerProvider(_ *tool.RunContext, _ ref.Locator) (provider.Provider, error) {
	return peerRecipe(), nil
}
