package lenscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// S18 slice: `dir:<path>@<host>` reads a directory tree on a sphere peer,
// read-only, through outpost's existing peer status + sftp transport. The host
// is only ever the one spelled on the locator (no env fallback); it must match
// exactly one peer by host or alias. Only ls/get are ever issued.

const hostMaxFile = 4 << 20

type hostEntry struct {
	Name    string
	Dir     bool
	Size    int64
	ModTime time.Time
}

// hostTransport is the read-only remote view; tests inject a fake.
type hostTransport interface {
	List(ctx context.Context, name, dir string) ([]hostEntry, error)
	Read(ctx context.Context, name, file string) ([]byte, error)
}

var hostTransportImpl hostTransport = outpostSFTP{}

type outpostSFTP struct{}

type hostSSHTarget struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

var hostTargetsRunner = runHostTargets

func runHostTargets(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, peerTimeout)
	defer cancel()
	var out peerCapture
	c := exec.CommandContext(ctx, "outpost", "ssh", "list", "--json")
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, fmt.Errorf("host: SSH target listing exceeds %d bytes", peerMaxOutput)
	}
	return out.Bytes(), nil
}

func (outpostSFTP) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, peerTimeout)
	defer cancel()
	var out peerCapture
	c := exec.CommandContext(ctx, "outpost", append([]string{"ssh", "sftp"}, args...)...)
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, fmt.Errorf("host: outpost output exceeds %d bytes", peerMaxOutput)
	}
	return out.Bytes(), nil
}

func (t outpostSFTP) List(ctx context.Context, name, dir string) ([]hostEntry, error) {
	raw, err := t.run(ctx, "ls", name, dir)
	if err != nil {
		return nil, err
	}
	return parseHostListing(raw)
}

// outpost prints mode, size, and the unescaped filename as tab-separated
// columns. Split only the first two tabs: filenames may contain spaces or tabs.
func parseHostListing(raw []byte) ([]hostEntry, error) {
	var es []hostEntry
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("host: malformed outpost listing")
		}
		n := f[2]
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\r\n\x00") {
			return nil, fmt.Errorf("host: unsafe filename in outpost listing")
		}
		// A remote symlink can point outside the requested tree. Never follow it.
		if strings.HasPrefix(f[0], "L") || strings.HasPrefix(f[0], "l") {
			continue
		}
		es = append(es, hostEntry{Name: n, Dir: strings.HasPrefix(f[0], "d")})
	}
	return es, nil
}

func (t outpostSFTP) Read(ctx context.Context, name, file string) ([]byte, error) {
	return t.run(ctx, "get", name, file)
}

// resolveHostPeer maps an explicit scope value to exactly one online peer and
// returns the name outpost addresses it by.
func resolveHostPeer(ctx context.Context, id string) (string, error) {
	name, _, err := resolveHostPeerEx(ctx, id)
	return name, err
}

// resolveHostPeerEx is resolveHostPeer plus the matched peer's host, so
// callers with host-based policy (the sandbox tier is never the local
// Dragon host) can enforce it even when the scope spelled an alias.
func resolveHostPeerEx(ctx context.Context, id string) (string, string, error) {
	peers, err := fetchPeers(ctx)
	if err != nil {
		return "", "", fmt.Errorf("@%s: %w", id, err)
	}
	var hits []peerStatus
	for _, p := range peers {
		if p.Host == id || p.Alias == id {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return "", "", fmt.Errorf("@%s: unknown peer", id)
	case 1:
		if !hits[0].Online {
			return "", "", fmt.Errorf("@%s: peer is offline", id)
		}
		if strings.HasPrefix(hits[0].Host, "-") {
			return "", "", fmt.Errorf("@%s: invalid peer host", id)
		}
		raw, err := hostTargetsRunner(ctx)
		if err != nil {
			return "", "", fmt.Errorf("@%s: list SSH targets: %w", id, err)
		}
		var targets []hostSSHTarget
		if err := json.Unmarshal(raw, &targets); err != nil {
			return "", "", fmt.Errorf("@%s: decode SSH targets: %w", id, err)
		}
		var names []string
		for _, target := range targets {
			if target.Host == hits[0].Host && target.Name != "" && !strings.HasPrefix(target.Name, "-") {
				names = append(names, target.Name)
			}
		}
		if len(names) != 1 {
			return "", "", fmt.Errorf("@%s: need exactly one saved SSH target for peer %s (found %d)", id, hits[0].Host, len(names))
		}
		return names[0], hits[0].Host, nil
	}
	hosts := make([]string, len(hits))
	for i, p := range hits {
		hosts[i] = p.Host
	}
	return "", "", fmt.Errorf("@%s: ambiguous peer, matches %s", id, strings.Join(hosts, ", "))
}

func hostPathOK(p string) error {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.ContainsRune(p, 0) || path.IsAbs(p) || strings.HasPrefix(p, "~") || strings.HasPrefix(p, "-") || strings.Contains(p, ":") {
		return fmt.Errorf("@host scope takes a host-relative path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%s: escapes host scope", p)
		}
	}
	return nil
}

func hostProvider(loc ref.Locator) (provider.Provider, error) {
	if loc.Scheme != "dir" {
		return nil, fmt.Errorf("provider %q does not support @host scope", loc.Scheme)
	}
	if len(loc.Inner) != 0 {
		return nil, fmt.Errorf("@host scope takes a host-relative path")
	}
	if err := hostPathOK(loc.Target); err != nil {
		return nil, err
	}
	name, err := resolveHostPeer(context.Background(), loc.ScopeValue)
	if err != nil {
		return nil, err
	}
	d := &provider.Dir{FS: &hostFS{name: name, t: hostTransportImpl}, Root: "."}
	return &hostScoped{inner: d, value: loc.ScopeValue}, nil
}

// hostFS is a read-only fs.FS over the transport.
type hostFS struct {
	name string
	t    hostTransport
}

func (h *hostFS) ReadDir(p string) ([]fs.DirEntry, error) {
	es, err := h.t.List(context.Background(), h.name, p)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(es))
	for i, e := range es {
		out[i] = fs.FileInfoToDirEntry(hostInfo{e})
	}
	return out, nil
}

func (h *hostFS) Stat(p string) (fs.FileInfo, error) {
	if p == "." {
		return hostInfo{hostEntry{Name: ".", Dir: true}}, nil
	}
	es, err := h.t.List(context.Background(), h.name, path.Dir(p))
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if e.Name == path.Base(p) {
			return hostInfo{e}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}

func (h *hostFS) Open(p string) (fs.File, error) {
	st, err := h.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fmt.Errorf("is a directory")}
	}
	b, err := h.t.Read(context.Background(), h.name, p)
	if err != nil {
		return nil, err
	}
	if len(b) > hostMaxFile {
		return nil, fmt.Errorf("%s: exceeds %d bytes", p, hostMaxFile)
	}
	return &hostFile{Reader: bytes.NewReader(b), info: hostInfo{hostEntry{Name: st.Name(), Size: int64(len(b))}}}, nil
}

type hostFile struct {
	*bytes.Reader
	info hostInfo
}

func (f *hostFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *hostFile) Close() error               { return nil }

type hostInfo struct{ e hostEntry }

func (i hostInfo) Name() string { return i.e.Name }
func (i hostInfo) Size() int64  { return i.e.Size }
func (i hostInfo) Mode() fs.FileMode {
	if i.e.Dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i hostInfo) ModTime() time.Time { return i.e.ModTime }
func (i hostInfo) IsDir() bool        { return i.e.Dir }
func (i hostInfo) Sys() any           { return nil }

// hostScoped strips the scope before the inner provider sees a locator and
// restores it on every emitted ref.
type hostScoped struct {
	inner provider.Provider
	value string
}

func (h *hostScoped) local(loc ref.Locator) (ref.Locator, error) {
	if loc.ScopeKind != "host" || loc.ScopeValue != h.value {
		return ref.Locator{}, provider.ErrUnsupported
	}
	if err := hostPathOK(loc.Target); err != nil {
		return ref.Locator{}, err
	}
	loc.ScopeKind, loc.ScopeValue = "", ""
	return loc, nil
}

func (h *hostScoped) scoped(s string) string {
	l, err := ref.ParseLocator(s)
	if err != nil {
		return s
	}
	l.ScopeKind, l.ScopeValue = "host", h.value
	return l.String()
}

func (h *hostScoped) Stat(ctx context.Context, loc ref.Locator) (provider.Node, error) {
	l, err := h.local(loc)
	if err != nil {
		return provider.Node{}, err
	}
	n, err := h.inner.Stat(ctx, l)
	n.Ref = h.scoped(n.Ref)
	return n, err
}

func (h *hostScoped) List(ctx context.Context, loc ref.Locator) ([]provider.Node, error) {
	l, err := h.local(loc)
	if err != nil {
		return nil, err
	}
	ns, err := h.inner.List(ctx, l)
	for i := range ns {
		ns[i].Ref = h.scoped(ns[i].Ref)
	}
	return ns, err
}

func (h *hostScoped) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	l, err := h.local(loc)
	if err != nil {
		return nil, err
	}
	return h.inner.Open(ctx, l)
}

func (h *hostScoped) Edges(ctx context.Context, loc ref.Locator) ([]provider.Edge, error) {
	l, err := h.local(loc)
	if err != nil {
		return nil, err
	}
	es, err := h.inner.Edges(ctx, l)
	for i := range es {
		es[i].Src, es[i].Dst = h.scoped(es[i].Src), h.scoped(es[i].Dst)
	}
	return es, err
}
