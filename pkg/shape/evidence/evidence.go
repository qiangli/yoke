// Package evidence is the sealed record of one verification run: what was
// verified, against which frozen inputs, by which verifier, for whom.
//
// Three properties carry the whole design.
//
// Outside the candidate. Records live under $BASHY_HOME/src/.evidence, never
// inside a tree a candidate can edit, so writing a candidate cannot write its
// own verdict.
//
// Sealed. Every record carries an HMAC-SHA256 seal over its own bytes, keyed
// by a host key the candidate never sees. An edited verdict, an edited
// binding, a hand-written record and a record copied from another host all
// fail the seal and are refused as forged — the reader never falls back to
// "unsealed but plausible".
//
// Bound. The seal covers a binding of {candidate SHA, base SHA, verifier
// build id, contract digest, dependency digest, principal}. A record proves
// a verdict for exactly that tuple: rebase the candidate, move the base,
// rebuild the verifier, edit the frozen contract, change the dependencies or
// ask on behalf of a different principal and the record is stale, not
// weaker evidence. Staleness is the normal case, not an error condition —
// re-verify.
//
// pkg/shape/verify is the only writer. Readers (the merge gate, activation)
// call Require with the binding they independently recomputed; nothing in
// the record is trusted to describe itself.
package evidence

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Schema is the record envelope name.
const Schema = "bashy-shape-evidence-v1"

// Verdicts. Only VerdictPass ever permits a merge or an activation.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
)

// Check statuses. Every required check yields exactly one of these, and only
// StatusPass counts towards a passing verdict: a skipped check is an unproven
// check, not a lenient one.
const (
	StatusPass    = "pass"
	StatusFail    = "fail"
	StatusSkip    = "skip"
	StatusTimeout = "timeout"
	StatusCancel  = "cancel"
)

// Sentinels for the four ways evidence can fail to authorize something.
// Callers match these with errors.Is to decide what to tell the operator:
// missing and stale mean "verify again", failed means "fix the candidate",
// and forged means something tried to mint a verdict it had not earned.
var (
	ErrMissing = errors.New("evidence: no record for this candidate")
	ErrForged  = errors.New("evidence: record seal does not verify")
	ErrStale   = errors.New("evidence: record is bound to different inputs")
	ErrFailed  = errors.New("evidence: recorded verdict is not a pass")
)

// Binding is what a record proves a verdict *about*. Every field is part of
// the seal; any difference between the binding a reader recomputes and the
// one in the record makes the record stale.
type Binding struct {
	// Candidate is the commit the verifier ran against.
	Candidate string `json:"candidate"`
	// Base is the commit the frozen contract and checks were read from.
	Base string `json:"base"`
	// Verifier is the build id of the verifier that produced the record.
	Verifier string `json:"verifier"`
	// Contract is the digest of the base-frozen contract and check set.
	Contract string `json:"contract"`
	// Dependency is the digest of the candidate's dependency manifests.
	Dependency string `json:"dependency"`
	// Principal is who the verdict is for, as resolved by the launcher gate.
	Principal string `json:"principal"`
}

// CheckResult is one required check's outcome.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Record is one sealed verification result.
type Record struct {
	Schema  string        `json:"schema"`
	Binding Binding       `json:"binding"`
	Verdict string        `json:"verdict"`
	Checks  []CheckResult `json:"checks"`
	// Uncalled lists contracted functions the check set never invoked. A
	// non-empty list is why an otherwise all-pass run still failed.
	Uncalled []string `json:"uncalled,omitempty"`
	// Reason explains a failing verdict in one line.
	Reason   string `json:"reason,omitempty"`
	IssuedAt string `json:"issued_at"`
	// Seal is the hex HMAC-SHA256 over the record with Seal emptied.
	Seal string `json:"seal"`
}

// Dir is $BASHY_HOME/src/.evidence — outside every candidate tree.
func Dir(home string) string { return filepath.Join(home, "src", ".evidence") }

// Path is the record file for one candidate commit.
func Path(home, candidate string) string {
	return filepath.Join(Dir(home), candidate+".json")
}

// HostKey resolves the host's seal key. It is a variable so a host whose
// secrets facility holds the key can supply it; the default mints one
// host-local key, owner-only, on first use. Replacing it invalidates every
// record minted under the previous key, which is the intended behaviour.
var HostKey = LocalHostKey

const hostKeyName = ".hostkey"

// LocalHostKey reads, or mints once, the host-local seal key: 32 random
// bytes in an owner-only file beside the records. It is deliberately not
// derivable from anything a candidate can read.
func LocalHostKey(home string) ([]byte, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("evidence: a bashy home is required")
	}
	dir := Dir(home)
	p := filepath.Join(dir, hostKeyName)
	if raw, err := os.ReadFile(p); err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) < 32 {
			return nil, fmt.Errorf("evidence: host key is corrupt; remove %s and re-verify", p)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	// O_EXCL so two verifiers racing on first use agree on one key.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LocalHostKey(home)
		}
		return nil, err
	}
	_, werr := f.WriteString(hex.EncodeToString(key) + "\n")
	cerr := f.Close()
	if werr != nil {
		return nil, werr
	}
	if cerr != nil {
		return nil, cerr
	}
	return key, nil
}

// seal computes the HMAC over the record's canonical bytes with Seal empty.
// Canonical = encoding/json of the struct: field order is the declaration
// order and every field is a scalar or an ordered slice, so the same record
// always seals the same way.
func seal(key []byte, rec Record) (string, error) {
	rec.Seal = ""
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	m.Write(raw)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// Write seals rec with the host key and stores it at Path(home, candidate).
// Only pkg/shape/verify calls this; a test pins that.
func Write(home string, rec Record) (string, error) {
	if rec.Binding.Candidate == "" {
		return "", errors.New("evidence: a record needs a candidate commit")
	}
	if rec.Verdict != VerdictPass && rec.Verdict != VerdictFail {
		return "", fmt.Errorf("evidence: unknown verdict %q", rec.Verdict)
	}
	rec.Schema = Schema
	if rec.IssuedAt == "" {
		rec.IssuedAt = time.Now().UTC().Format(time.RFC3339)
	}
	key, err := HostKey(home)
	if err != nil {
		return "", err
	}
	s, err := seal(key, rec)
	if err != nil {
		return "", err
	}
	rec.Seal = s
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(Dir(home), 0o700); err != nil {
		return "", err
	}
	p := Path(home, rec.Binding.Candidate)
	// Rename a whole file so a reader never sees a half-written record.
	tmp, err := os.CreateTemp(Dir(home), ".record-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, p)
	}
	if werr != nil {
		os.Remove(name)
		return "", werr
	}
	return p, nil
}

// Read returns the record for candidate with its seal verified. A record
// whose seal does not verify, or whose sealed candidate is not the one the
// filename claims, is ErrForged — never a weaker result.
func Read(home, candidate string) (Record, error) {
	raw, err := os.ReadFile(Path(home, candidate))
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, fmt.Errorf("%w: %s", ErrMissing, shortSHA(candidate))
		}
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("%w: %s is not a record", ErrForged, shortSHA(candidate))
	}
	key, err := HostKey(home)
	if err != nil {
		return Record{}, err
	}
	want, err := seal(key, rec)
	if err != nil {
		return Record{}, err
	}
	if rec.Seal == "" || !hmac.Equal([]byte(want), []byte(rec.Seal)) {
		return Record{}, fmt.Errorf("%w: %s", ErrForged, shortSHA(candidate))
	}
	if rec.Schema != Schema {
		return Record{}, fmt.Errorf("%w: unknown schema %q", ErrForged, rec.Schema)
	}
	// The filename is a claim; the seal is the fact. Refuse a record that
	// was moved onto another candidate's name.
	if rec.Binding.Candidate != candidate {
		return Record{}, fmt.Errorf("%w: record is for %s, not %s",
			ErrStale, shortSHA(rec.Binding.Candidate), shortSHA(candidate))
	}
	return rec, nil
}

// Require returns the record authorizing want, or the reason it does not.
// Every non-empty field of want must match the sealed binding exactly; an
// empty field means the caller cannot recompute that part and is not
// checking it. The verdict must be a pass.
func Require(home string, want Binding) (Record, error) {
	rec, err := Read(home, want.Candidate)
	if err != nil {
		return Record{}, err
	}
	if mismatch := rec.Binding.differs(want); mismatch != "" {
		return Record{}, fmt.Errorf("%w: %s", ErrStale, mismatch)
	}
	if rec.Verdict != VerdictPass {
		reason := rec.Reason
		if reason == "" {
			reason = failedChecks(rec)
		}
		return Record{}, fmt.Errorf("%w: %s", ErrFailed, reason)
	}
	return rec, nil
}

// differs names the first field of want that the binding does not satisfy,
// or "" when it satisfies all of them. Empty want fields are not checked.
func (b Binding) differs(want Binding) string {
	for _, f := range []struct{ name, got, want string }{
		{"candidate", b.Candidate, want.Candidate},
		{"base", b.Base, want.Base},
		{"verifier build", b.Verifier, want.Verifier},
		{"contract digest", b.Contract, want.Contract},
		{"dependency digest", b.Dependency, want.Dependency},
		{"principal", b.Principal, want.Principal},
	} {
		if f.want == "" || f.got == f.want {
			continue
		}
		return fmt.Sprintf("%s is %s, the record is for %s",
			f.name, shortSHA(f.want), shortSHA(f.got))
	}
	return ""
}

// failedChecks summarises why a recorded verdict is not a pass.
func failedChecks(rec Record) string {
	var bad []string
	for _, c := range rec.Checks {
		if c.Status != StatusPass {
			bad = append(bad, c.Name+" "+c.Status)
		}
	}
	if len(rec.Uncalled) > 0 {
		bad = append(bad, "never called: "+strings.Join(rec.Uncalled, ", "))
	}
	if len(bad) == 0 {
		return "verdict " + rec.Verdict
	}
	return strings.Join(bad, "; ")
}

// shortSHA abbreviates a commit for messages and leaves other values alone.
func shortSHA(s string) string {
	if len(s) == 40 && isHex(s) {
		return s[:8]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
