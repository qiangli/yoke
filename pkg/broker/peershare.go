package broker

import (
	"context"
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

	"github.com/qiangli/yoke/pkg/boundary"
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
	// Email binds this credential to one Cloudbox member. Circle optionally
	// narrows that member to the owner's current circle label.
	Email  string `json:"email,omitempty"`
	Circle string `json:"circle,omitempty"`
	// Limits caps the peer's usage (nil = unlimited); see peerlimit.go.
	Limits *PeerLimits `json:"limits,omitempty"`
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
	return MintPeerCredentialFor(path, name, share, "", "", now)
}

// MintPeerCredentialFor is MintPeerCredential plus an optional membership
// binding. Empty email/circle preserve an existing binding during rotation.
func MintPeerCredentialFor(path, name, share, email, circle string, now time.Time) (string, error) {
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
	if email != "" {
		rec.Email = strings.ToLower(strings.TrimSpace(email))
		rec.Circle = strings.TrimSpace(circle)
	}
	rec.Hash = hashCredential(cred)
	ps.Peers[name] = rec
	return cred, savePeerShares(path, ps)
}

// SetPeerMember changes an existing peer's member binding. Empty email clears
// the binding and restores legacy owner-tool behavior.
func SetPeerMember(path, name, email, circle string, now time.Time) error {
	ps, err := LoadPeerShares(path)
	if err != nil {
		return err
	}
	rec, ok := ps.Peers[name]
	if !ok {
		return fmt.Errorf("broker: no peer %q (mint one with `llm peer add %s`)", name, name)
	}
	rec.Email = strings.ToLower(strings.TrimSpace(email))
	rec.Circle = strings.TrimSpace(circle)
	if rec.Email == "" {
		rec.Circle = ""
	}
	rec.Updated = now
	ps.Peers[name] = rec
	return savePeerShares(path, ps)
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

// MemberAuthorizer supplies active+mutual Cloudbox membership decisions.
type MemberAuthorizer interface {
	Decide(context.Context, string, []string) (boundary.Decision, error)
}

type peerAuthorization struct {
	principal string
	limits    *PeerLimits
	member    *boundary.Decision
}

type peerMemberRefusal struct{ reason string }

func (e *peerMemberRefusal) Error() string { return e.reason }

// authorizePeer matches presented against the stored peer hashes. It returns
// ("", nil) when no peer matches, so the caller reports an unknown bearer.
// The file is read per request so share/revoke/limit take effect without
// restart; the matched peer's limits come back with it.
func (b *Broker) authorizePeer(ctx context.Context, presented string) (peerAuthorization, error) {
	if b.opts.PeersFile == "" {
		return peerAuthorization{}, nil
	}
	ps, err := LoadPeerShares(b.opts.PeersFile)
	if err != nil {
		return peerAuthorization{}, errors.New("peer policy unreadable; refusing peer credentials")
	}
	got := hashCredential(presented)
	var match string
	for _, name := range ps.Names() {
		if subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(ps.Peers[name].Hash))) == 1 {
			match = name
		}
	}
	if match == "" {
		return peerAuthorization{}, nil
	}
	auth := peerAuthorization{principal: PeerPrincipalPrefix + match, limits: ps.Peers[match].Limits}
	if ps.Peers[match].Share != ShareAllow {
		return auth, errPeerNotShared
	}
	rec := ps.Peers[match]
	if rec.Email == "" {
		return auth, nil
	}
	if b.opts.Members == nil {
		return auth, &peerMemberRefusal{reason: "member list unavailable; refusing member-bound peer"}
	}
	grants := []string{rec.Email}
	if rec.Circle != "" {
		grants = []string{rec.Circle}
	}
	decision, err := b.opts.Members.Decide(ctx, rec.Email, grants)
	if err == nil || decision.Offline {
		auth.member = &decision
	}
	if err != nil {
		return auth, &peerMemberRefusal{reason: fmt.Sprintf("member check unavailable; refusing %s: %v", auth.principal, err)}
	}
	if !decision.Allowed {
		return auth, &peerMemberRefusal{reason: fmt.Sprintf("%s is not an active mutual member allowed by this peer grant", rec.Email)}
	}
	return auth, nil
}

// peerRouteAllowed is the whole surface a shared peer may reach: inference
// (including slash / capability tool-command runs), the model listing (so a
// peer's client can discover the served ids) and health. Sticky management,
// the native engine API and cligw's own surface stay owner-only.
func peerRouteAllowed(method, path string) bool {
	switch path {
	case "/health", "/v1/models", "/openai/v1/models":
		return method == "GET"
	}
	if method != "POST" {
		return false
	}
	return isInferencePath(path)
}
