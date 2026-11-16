package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/qiangli/yoke/pkg/coopauth"
)

// Self-service member keys (Sprint 648). A Cloudbox member signed in through
// the satellite calls POST /v1/peer/self on the door's public app; the outpost
// stamps the caller's email with a signed identity (trust_cloud_identity), the
// door checks the email is an active mutual member of an allowed circle, and
// mints (or rotates) the peer "self-<email>" bound to that email and circle.
// Every inference request then re-checks membership, so removing the member
// from the circle disables the key. Revoke is for a leaked key only: the
// member may mint again while still in an allowed circle.

// SelfPeerPrefix prefixes every self-service peer name.
const SelfPeerPrefix = "self-"

// SelfServicePath is the self-service route on the door.
const SelfServicePath = "/v1/peer/self"

// SelfService is the owner's opt-in, stored beside the peers file.
type SelfService struct {
	Enabled bool     `json:"enabled"`
	Circles []string `json:"circles,omitempty"`
	// SSOSecret verifies the outpost's signed identity stamp on the door's app.
	SSOSecret string    `json:"sso_secret,omitempty"`
	Updated   time.Time `json:"updated,omitempty"`
}

var selfEmailRE = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+$`)

// SelfPeerName is the peer name a member's self-service key carries.
func SelfPeerName(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > 254 || !selfEmailRE.MatchString(email) {
		return "", fmt.Errorf("broker: %q is not a usable member email", email)
	}
	return SelfPeerPrefix + email, nil
}

func validSelfPeerName(name string) bool {
	email, ok := strings.CutPrefix(name, SelfPeerPrefix)
	if !ok {
		return false
	}
	got, err := SelfPeerName(email)
	return err == nil && got == name
}

// SelfServiceFile is the self-service policy beside the peers file.
func SelfServiceFile(peersFile string) string {
	return filepath.Join(filepath.Dir(peersFile), "self-service.json")
}

// LoadSelfService reads path; a missing file is self-service off.
func LoadSelfService(path string) (SelfService, error) {
	var ss SelfService
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ss, nil
	}
	if err != nil {
		return ss, err
	}
	if err := json.Unmarshal(data, &ss); err != nil {
		return SelfService{}, fmt.Errorf("broker: invalid self-service file: %w", err)
	}
	return ss, nil
}

// SaveSelfService writes the policy owner-only.
func SaveSelfService(path string, ss SelfService) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ss, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(data, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// Self-service refusal codes; the CLI maps each to one line naming the fix.
const (
	SelfCodeOff        = "self_service_off"
	SelfCodeNotSigned  = "not_signed_in"
	SelfCodePending    = "invitation_pending"
	SelfCodeNotMember  = "not_a_member"
	SelfCodeWrongCirc  = "circle_not_allowed"
	SelfCodeUnverified = "member_check_unavailable"
)

type selfRefusal struct {
	status int
	code   string
	msg    string
}

// selfServiceMint decides and performs one self-service request for email
// (already verified). It returns the peer name and fresh credential.
func selfServiceMint(ctx context.Context, peersFile string, members MemberAuthorizer, email string, now time.Time) (string, string, *selfRefusal) {
	ss, err := LoadSelfService(SelfServiceFile(peersFile))
	if err != nil || !ss.Enabled || len(ss.Circles) == 0 {
		return "", "", &selfRefusal{http.StatusForbidden, SelfCodeOff, "self-service is off on this door; ask the owner to run `bashy llm self-service on --circle C`"}
	}
	if email == "" {
		return "", "", &selfRefusal{http.StatusUnauthorized, SelfCodeNotSigned, "not signed in: reach this door through the satellite with your Cloudbox account"}
	}
	name, err := SelfPeerName(email)
	if err != nil {
		return "", "", &selfRefusal{http.StatusForbidden, SelfCodeNotMember, err.Error()}
	}
	lister, ok := members.(memberLister)
	if members == nil || !ok {
		return "", "", &selfRefusal{http.StatusServiceUnavailable, SelfCodeUnverified, "member list unavailable on this door"}
	}
	rows, stale, _, err := lister.ListWithAge(ctx)
	if err != nil {
		return "", "", &selfRefusal{http.StatusServiceUnavailable, SelfCodeUnverified, "member check unavailable: " + err.Error()}
	}
	// Minting creates an authorization record, so it fails closed offline
	// (the 24h grace is for serving already-issued keys).
	if stale {
		return "", "", &selfRefusal{http.StatusServiceUnavailable, SelfCodeUnverified, "member check unavailable: the account master is unreachable"}
	}
	circle, refusal := selfEligible(email, ss.Circles, rows)
	if refusal != nil {
		return "", "", refusal
	}
	cred, err := mintSelfPeer(peersFile, name, email, circle, now)
	if err != nil {
		return "", "", &selfRefusal{http.StatusInternalServerError, "mint_failed", "could not mint: " + err.Error()}
	}
	return name, cred, nil
}

// memberLister is the member client's list surface (boundary.Client).
type memberLister interface {
	ListWithAge(context.Context) ([]boundary.Member, bool, time.Duration, error)
}

// selfEligible returns the allowed circle email is an active mutual member
// of, or the refusal that tells the member what to fix.
func selfEligible(email string, circles []string, rows []boundary.Member) (string, *selfRefusal) {
	email = strings.ToLower(strings.TrimSpace(email))
	var row *boundary.Member
	for i := range rows {
		if strings.EqualFold(rows[i].Email, email) {
			row = &rows[i]
			break
		}
	}
	switch {
	case row == nil:
		return "", &selfRefusal{http.StatusForbidden, SelfCodeNotMember, email + " is not a member of this door owner's account; ask the owner for an invitation"}
	case row.Status != "active" || !row.Mutual:
		return "", &selfRefusal{http.StatusForbidden, SelfCodePending, email + " has a pending invitation; accept it in Cloudbox, then run this again"}
	}
	for _, c := range circles {
		if c == row.Circle {
			return c, nil
		}
	}
	return "", &selfRefusal{http.StatusForbidden, SelfCodeWrongCirc, fmt.Sprintf("%s is in circle %q, which this door does not allow for self-service", email, row.Circle)}
}

// serveSelfService answers POST /v1/peer/self. It needs no door credential:
// the caller is the outpost-signed Cloudbox identity.
func (b *Broker) serveSelfService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErrCode(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST "+SelfServicePath)
		return
	}
	if b.opts.PeersFile == "" {
		writeErrCode(w, http.StatusForbidden, SelfCodeOff, "self-service is off on this door")
		return
	}
	ss, _ := LoadSelfService(SelfServiceFile(b.opts.PeersFile))
	email := ""
	if ss.SSOSecret != "" && coopauth.ArrivedViaCloud(r) && coopauth.VerifyOutpost(r, []byte(ss.SSOSecret)) {
		email = strings.ToLower(strings.TrimSpace(r.Header.Get(coopauth.HdrRemoteUser)))
	}
	name, cred, refusal := selfServiceMint(r.Context(), b.opts.PeersFile, b.opts.Members, email, b.opts.Now().UTC())
	if refusal != nil {
		slog.Info("llm: self-service refused", "email", email, "code", refusal.code)
		writeErrCode(w, refusal.status, refusal.code, refusal.msg)
		return
	}
	slog.Info("llm: self-service key issued", "principal", PeerPrincipalPrefix+name)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"peer": PeerPrincipalPrefix + name, "credential": cred})
}

func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
