package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Per-peer door credentials (Sprint 418 S9; the bounded slice of Sprint 308
// A3/A5 this door needs). The owner mints a credential for a named peer
// principal; only its SHA-256 is stored, beside the owner token. A request
// carrying it authenticates as "peer:<name>" and is admitted only while that
// peer's share policy is "allow" — minting does not share (deny by default),
// and revoking removes the credential outright. The owner token is unchanged
// and is never a share.

// PeerPrincipalPrefix prefixes every peer principal.
const PeerPrincipalPrefix = "peer:"

// Share policies.
const (
	ShareAllow = "allow"
	ShareDeny  = "deny"
)

// PeerShare is one peer's stored record. Hash is hex(sha256(credential)).
type PeerShare struct {
	Hash    string    `json:"sha256"`
	Share   string    `json:"share"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated,omitempty"`
}

// PeerShares is the on-disk peers file.
type PeerShares struct {
	Peers map[string]PeerShare `json:"peers"`
}

var peerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidPeerName reports whether name may name a peer principal.
func ValidPeerName(name string) bool { return peerNameRE.MatchString(name) }

func hashCredential(cred string) string {
	sum := sha256.Sum256([]byte(cred))
	return hex.EncodeToString(sum[:])
}

// LoadPeerShares reads path; a missing file is an empty policy.
func LoadPeerShares(path string) (PeerShares, error) {
	ps := PeerShares{Peers: map[string]PeerShare{}}
	if path == "" {
		return ps, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ps, nil
	}
	if err != nil {
		return ps, err
	}
	if err := json.Unmarshal(data, &ps); err != nil {
		return PeerShares{Peers: map[string]PeerShare{}}, fmt.Errorf("broker: invalid peers file: %w", err)
	}
	if ps.Peers == nil {
		ps.Peers = map[string]PeerShare{}
	}
	return ps, nil
}

func savePeerShares(path string, ps PeerShares) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MintPeerCredential creates (or rotates) name's credential and returns it.
// The share policy starts at deny unless share says otherwise; rotating keeps
// the existing policy. The credential is returned once and never stored.
func MintPeerCredential(path, name, share string, now time.Time) (string, error) {
	if !ValidPeerName(name) {
		return "", fmt.Errorf("broker: invalid peer name %q (want [a-z0-9][a-z0-9._-]*)", name)
	}
	ps, err := LoadPeerShares(path)
	if err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	cred := "bpeer_" + hex.EncodeToString(raw)
	rec, existed := ps.Peers[name]
	if !existed {
		rec = PeerShare{Share: ShareDeny, Created: now}
	} else {
		rec.Updated = now
	}
	if share != "" {
		if share != ShareAllow && share != ShareDeny {
			return "", fmt.Errorf("broker: share must be %s or %s", ShareAllow, ShareDeny)
		}
		rec.Share = share
	}
	rec.Hash = hashCredential(cred)
	ps.Peers[name] = rec
	return cred, savePeerShares(path, ps)
}

// SetPeerShare sets an existing peer's share policy.
func SetPeerShare(path, name, share string, now time.Time) error {
	if share != ShareAllow && share != ShareDeny {
		return fmt.Errorf("broker: share must be %s or %s", ShareAllow, ShareDeny)
	}
	ps, err := LoadPeerShares(path)
	if err != nil {
		return err
	}
	rec, ok := ps.Peers[name]
	if !ok {
		return fmt.Errorf("broker: no peer %q (mint one with `llm peer add %s`)", name, name)
	}
	rec.Share, rec.Updated = share, now
	ps.Peers[name] = rec
	return savePeerShares(path, ps)
}

// RevokePeer deletes name's credential; a request carrying it is then an
// unknown bearer (401).
func RevokePeer(path, name string) error {
	ps, err := LoadPeerShares(path)
	if err != nil {
		return err
	}
	if _, ok := ps.Peers[name]; !ok {
		return fmt.Errorf("broker: no peer %q", name)
	}
	delete(ps.Peers, name)
	return savePeerShares(path, ps)
}

// Names lists the peers, sorted.
func (ps PeerShares) Names() []string {
	out := make([]string, 0, len(ps.Peers))
	for n := range ps.Peers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// errPeerNotShared is a known peer whose share policy does not admit it.
var errPeerNotShared = errors.New("peer credential is valid but this door does not share with that peer (share: deny); the owner can run `llm peer share NAME allow`")

// authorizePeer matches presented against the stored peer hashes. It returns
// ("", nil) when no peer matches, so the caller reports an unknown bearer.
// The file is read per request so share/revoke take effect without restart.
func (b *Broker) authorizePeer(presented string) (string, error) {
	if b.opts.PeersFile == "" {
		return "", nil
	}
	ps, err := LoadPeerShares(b.opts.PeersFile)
	if err != nil {
		return "", errors.New("peer policy unreadable; refusing peer credentials")
	}
	got := hashCredential(presented)
	var match string
	for _, name := range ps.Names() {
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(ps.Peers[name].Hash))) == 1 {
			match = name
		}
	}
	if match == "" {
		return "", nil
	}
	if ps.Peers[match].Share != ShareAllow {
		return PeerPrincipalPrefix + match, errPeerNotShared
	}
	return PeerPrincipalPrefix + match, nil
}

// peerRouteAllowed is the whole surface a shared peer may reach: inference
// (including slash / capability tool-command runs) and health. Sticky
// management, the native engine API and cligw's own surface stay owner-only.
func peerRouteAllowed(method, path string) bool {
	if path == "/health" {
		return method == "GET"
	}
	return method == "POST" && isInferencePath(path)
}
