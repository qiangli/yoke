// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pairing swaps the operator's OS account password for a single-use,
// time-boxed, device-scoped credential.
//
// WHY IT EXISTS. `bashy app serve --bind <lan-ip>` already works, and binding
// off-loopback already demands a login (gate.go switches the ungated-loopback
// row off). But the only LAN credential is HOST OS AUTH — the operator's own
// account password, typed into a phone browser and POSTed over plaintext HTTP.
// That is the highest-value reusable secret on the machine, spent to check a
// build from the couch. A QR carries a secret that is worth exactly one
// session, on one device, for a bounded time.
//
// WHAT IT IS NOT. Pairing stops the OS PASSWORD from crossing the wire; it does
// not make the link private. The device cookie is still sniffable on a
// plaintext LAN. That is a reasonable trade on a home network and an
// unreasonable one on a café's, and this file says so rather than implying
// otherwise. The QR payload is VERSIONED precisely so a v2 can carry a
// self-signed certificate fingerprint for the phone to pin — a QR is an
// out-of-band channel, which is what makes a pinned cert possible without a CA.
//
// WHY A FILE AND NOT AN API. `bashy app pair` is a different process from
// `bashy app serve`, and the server has no ungated loopback route to offer it
// (that row is off precisely because the console is LAN-bound). A 0600 file
// under the console's own state directory is the smallest thing that both
// processes can agree on, and it is the same mechanism `session.key` already
// uses.
//
// THE FILE NEVER HOLDS A USABLE TICKET. Only sha256(secret) is written; the
// secret exists in the QR and in the phone's request. Reading the file gives
// an attacker the ability to see that a pairing happened, not to perform one.

const pairSchema = "bashy-apps-pairing-v1"

// pairQRVersion is the payload version carried in the QR's `v` parameter.
// Bump it when the payload gains a field a redeemer must understand — a v2
// carrying a certificate fingerprint is the planned next step.
const pairQRVersion = "1"

// defaultTicketWindow is how long an unredeemed ticket stays valid. Short
// because it only has to survive the walk from the terminal to the phone.
const defaultTicketWindow = 2 * time.Minute

// defaultDeviceTTL is how long a paired device keeps access when no TTL is
// given.
//
// A day rather than the original four hours. The four-hour figure was chosen
// for a pairing you do once to try something; the actual use is a phone that
// stays paired to the machine on the desk, and re-scanning a QR every half
// working day is a cost paid every day to bound a risk that has not changed —
// the device is scoped (no terminal, no files), single-use to obtain, and
// revocable at any time from the same page that minted it.
const defaultDeviceTTL = 24 * time.Hour

// neverExpiresTTL is what "never expire" actually stores.
//
// A century, not a zero and not a null. Every reader of a device record —
// `apps pair list`, the Settings table, the gate — compares against a time, and
// a sentinel would have to be understood by all of them; the one that forgot
// would treat "never" as "expired at the epoch" and lock the phone out, or as
// "always valid" for a record that really had run out. A date past every
// machine that will ever read it needs no special case anywhere.
const neverExpiresTTL = 100 * 365 * 24 * time.Hour

// neverExpiresAfter is the threshold at which a stored expiry is DISPLAYED as
// "never" rather than as a date in 2126. Anything beyond ten years was asked
// for, not accumulated.
const neverExpiresAfter = 10 * 365 * 24 * time.Hour

// operatorGrantTTL is the ceiling a pairing may inherit: the same 12h an
// interactive operator login gets. A device session inherits but never exceeds
// the operator authority that minted it — UNLESS the operator explicitly asks
// for longer, which is what issueTicket's grant extension is for. See there.
const operatorGrantTTL = sessionTTLSeconds * time.Second

// deviceSubjectSep separates the OS user from the device id inside a session
// cookie's subject. It is '#' rather than '|' because websession reserves '|'
// as its own field separator and refuses a subject containing one.
const deviceSubjectSep = "#dev:"

var (
	errNoTicket    = errors.New("pairing ticket not recognised")
	errTicketUsed  = errors.New("pairing ticket has already been used")
	errTicketStale = errors.New("pairing ticket has expired")
)

// pairState is the whole on-disk document.
type pairState struct {
	Schema  string    `json:"schema"`
	Grant   *grant    `json:"grant,omitempty"`
	Tickets []ticket  `json:"tickets,omitempty"`
	Devices []device  `json:"devices,omitempty"`
	Updated time.Time `json:"updated"`
}

// grant is the operator authority a pairing hangs off.
//
// "Revoking the operator session revokes the devices" is implemented here
// rather than left as prose: every ticket and device records the grant id it
// was minted under, and a device is live only while its grant is. `apps revoke
// --all` ends the grant, which ends every device derived from it in one write.
type grant struct {
	ID      string    `json:"id"`
	Issued  time.Time `json:"issued"`
	Expires time.Time `json:"expires"`
	Revoked bool      `json:"revoked,omitempty"`
}

func (g *grant) live(now time.Time) bool {
	return g != nil && !g.Revoked && now.Before(g.Expires)
}

// ticket is one outstanding QR. Single-use: Redeemed closes it, and so does
// Expires — whichever comes first.
type ticket struct {
	ID       string     `json:"id"`
	Hash     string     `json:"hash"` // sha256(secret); the secret is never stored
	Grant    string     `json:"grant"`
	Scope    []string   `json:"scope"`
	TTL      string     `json:"ttl"` // the device TTL this ticket will confer
	Issued   time.Time  `json:"issued"`
	Expires  time.Time  `json:"expires"`
	Redeemed *time.Time `json:"redeemed,omitempty"`
	DeviceID string     `json:"device_id,omitempty"`
}

func (t ticket) open(now time.Time) bool {
	return t.Redeemed == nil && now.Before(t.Expires)
}

// device is a redeemed pairing: one phone, one scope, one deadline.
type device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Grant     string    `json:"grant"`
	Scope     []string  `json:"scope"`
	Issued    time.Time `json:"issued"`
	Expires   time.Time `json:"expires"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
	Revoked   bool      `json:"revoked,omitempty"`
	RevokedAt time.Time `json:"revoked_at,omitempty"`
}

// pairStore is the file-backed pairing state, safe for one process. Two
// processes coordinate through the file itself: every mutation is
// read-modify-write under an exclusive lock file, and the server re-reads on
// mtime change.
type pairStore struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	cached *pairState
	mod    time.Time
	size   int64
}

// pairPath is the pairing document's location, following the same ladder as
// the session key so relocating $BASHY_HOME relocates the pairing state with
// it — and so a test can never pair a developer's own console.
func pairPath() (string, error) {
	dir, err := serviceDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "pairing.json"), nil
}

func newPairStore(path string) *pairStore {
	return &pairStore{path: path, now: time.Now}
}

// openPairStore builds the default store.
func openPairStore() (*pairStore, error) {
	p, err := pairPath()
	if err != nil {
		return nil, err
	}
	return newPairStore(p), nil
}

// load returns the current state, re-reading only when the file changed.
func (s *pairStore) load() (*pairState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *pairStore) loadLocked() (*pairState, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &pairState{Schema: pairSchema}, nil
		}
		return nil, err
	}
	if s.cached != nil && fi.ModTime().Equal(s.mod) && fi.Size() == s.size {
		return s.cached, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	st := &pairState{Schema: pairSchema}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("pairing store %s is unreadable: %w", s.path, err)
		}
	}
	s.cached, s.mod, s.size = st, fi.ModTime(), fi.Size()
	return st, nil
}

// mutate is the only writer. It re-reads under the lock so a concurrent `apps
// pair` and a redeeming server never lose each other's write.
func (s *pairStore) mutate(fn func(*pairState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()

	// Drop the cache: another process may have written since we last read.
	s.cached = nil
	st, err := s.loadLocked()
	if err != nil {
		return err
	}
	// Copy so a failing fn cannot leave a half-applied state cached.
	next := *st
	next.Tickets = append([]ticket(nil), st.Tickets...)
	next.Devices = append([]device(nil), st.Devices...)
	if st.Grant != nil {
		g := *st.Grant
		next.Grant = &g
	}
	if err := fn(&next); err != nil {
		return err
	}
	next.Schema = pairSchema
	next.Updated = s.now().UTC()
	next.prune(s.now())

	raw, err := json.MarshalIndent(&next, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.cached = nil
	return nil
}

// lockFile takes an exclusive advisory lock via O_EXCL create, retrying
// briefly. Deliberately not flock: this must behave the same on Windows, where
// the console is a shipping target.
func (s *pairStore) lockFile() (func(), error) {
	lock := s.path + ".lock"
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		// A lock left behind by a crashed process must not wedge pairing
		// forever; 30s is far longer than any mutation here takes.
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > 30*time.Second {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("pairing store is locked by another process (%s)", lock)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// prune drops records that can never matter again, so the file does not grow
// without bound. A revoked or expired DEVICE is kept for a day so `apps
// devices` can still explain what happened; a closed ticket is dropped once it
// is well past its window.
func (st *pairState) prune(now time.Time) {
	tickets := st.Tickets[:0]
	for _, t := range st.Tickets {
		if t.open(now) || now.Sub(t.Expires) < time.Hour {
			tickets = append(tickets, t)
		}
	}
	st.Tickets = tickets

	devices := st.Devices[:0]
	for _, d := range st.Devices {
		if now.Sub(d.Expires) < 24*time.Hour {
			devices = append(devices, d)
		}
	}
	st.Devices = devices
}

// liveDevices returns the devices that would be admitted right now.
func (st *pairState) liveDevices(now time.Time) []device {
	var out []device
	for _, d := range st.Devices {
		if st.deviceLive(d, now) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issued.Before(out[j].Issued) })
	return out
}

func (st *pairState) deviceLive(d device, now time.Time) bool {
	if d.Revoked || !now.Before(d.Expires) {
		return false
	}
	// The grant is the operator authority; a device cannot outlive it.
	return st.Grant != nil && st.Grant.ID == d.Grant && st.Grant.live(now)
}

// openTickets counts the pairings still in flight — a LAN listener opened for
// pairing must stay up long enough for an outstanding QR to be scanned.
func (st *pairState) openTickets(now time.Time) int {
	n := 0
	for _, t := range st.Tickets {
		if t.open(now) {
			n++
		}
	}
	return n
}

// findDevice returns a live device by id.
func (st *pairState) findDevice(id string, now time.Time) (device, bool) {
	for _, d := range st.Devices {
		if d.ID == id {
			return d, st.deviceLive(d, now)
		}
	}
	return device{}, false
}

// ---------------------------------------------------------------------------
// mutations
// ---------------------------------------------------------------------------

// issueTicket mints a ticket and returns the SECRET, which is never persisted.
// The device TTL is min(ttl, remaining grant) — a paired phone inherits the
// operator's authority and can never exceed it.
func (s *pairStore) issueTicket(scope []string, ttl, window time.Duration) (ticket, string, error) {
	if ttl <= 0 {
		ttl = defaultDeviceTTL
	}
	if window <= 0 {
		window = defaultTicketWindow
	}
	secret, err := randToken(32)
	if err != nil {
		return ticket{}, "", err
	}
	now := s.now().UTC()
	var out ticket
	err = s.mutate(func(st *pairState) error {
		if !st.Grant.live(now) {
			id, err := randToken(9)
			if err != nil {
				return err
			}
			st.Grant = &grant{ID: id, Issued: now, Expires: now.Add(operatorGrantTTL)}
		}
		deviceExp := now.Add(ttl)
		if deviceExp.After(st.Grant.Expires) {
			// THE OPERATOR ASKED FOR LONGER THAN THEIR OWN GRANT RUNS.
			//
			// Clamping silently is what this used to do, and it made a longer
			// TTL a setting that appeared to work: a 24-hour pairing came back
			// as 24 hours in the response and expired with the 12-hour grant,
			// with nothing anywhere saying why the phone stopped working
			// overnight. Extending is the honest reading of the request — the
			// operator is deliberately granting a device authority that outlives
			// the session they granted it from, which is exactly what "keep my
			// phone paired" means — and it stays revocable: `apps pair revoke`
			// (and the Settings list) ends a device whenever the operator wants,
			// which is a better control than an expiry nobody chose.
			st.Grant.Expires = deviceExp
		}
		id, err := randToken(6)
		if err != nil {
			return err
		}
		out = ticket{
			ID:    id,
			Hash:  hashSecret(secret),
			Grant: st.Grant.ID,
			Scope: normalizeScope(scope),
			// Exact, NOT rounded. Rounding to the second turned a sub-second
			// TTL into "0s", which redeem then read as unparseable and
			// replaced with the four-hour default — silently WIDENING a grant
			// the operator had narrowed. A TTL may only ever be narrowed on
			// its way through this file.
			TTL:     deviceExp.Sub(now).String(),
			Issued:  now,
			Expires: now.Add(window),
		}
		st.Tickets = append(st.Tickets, out)
		return nil
	})
	if err != nil {
		return ticket{}, "", err
	}
	return out, secret, nil
}

// redeem turns a secret into a device. Single-use in both directions: the
// ticket closes and the window closes, whichever the caller hits first.
func (s *pairStore) redeem(secret, name, userAgent string) (device, error) {
	now := s.now().UTC()
	want := hashSecret(secret)
	var out device
	err := s.mutate(func(st *pairState) error {
		for i := range st.Tickets {
			t := &st.Tickets[i]
			if t.Hash != want {
				continue
			}
			if t.Redeemed != nil {
				return errTicketUsed
			}
			if !now.Before(t.Expires) {
				return errTicketStale
			}
			if st.Grant == nil || st.Grant.ID != t.Grant || !st.Grant.live(now) {
				return fmt.Errorf("the operator session that issued this ticket has ended")
			}
			id, err := randToken(8)
			if err != nil {
				return err
			}
			// A TTL this code cannot read is a REFUSAL, never a default. The
			// alternative — falling back to defaultDeviceTTL — turns a
			// corrupt or truncated record into a longer grant than anyone
			// asked for, which is the one direction this must never fail in.
			ttl, perr := time.ParseDuration(t.TTL)
			if perr != nil || ttl <= 0 {
				return fmt.Errorf("this pairing ticket carries an unreadable lifetime (%q); generate a fresh one", t.TTL)
			}
			exp := now.Add(ttl)
			if exp.After(st.Grant.Expires) {
				exp = st.Grant.Expires
			}
			out = device{
				ID: id, Name: name, Grant: t.Grant,
				Scope:  append([]string(nil), t.Scope...),
				Issued: now, Expires: exp, LastSeen: now,
				UserAgent: userAgent,
			}
			st.Devices = append(st.Devices, out)
			t.Redeemed = &now
			t.DeviceID = id
			return nil
		}
		return errNoTicket
	})
	if err != nil {
		return device{}, err
	}
	return out, nil
}

// touch records that a device was seen. Best-effort: a failed write must never
// fail the request it was observing.
func (s *pairStore) touch(id string) {
	now := s.now().UTC()
	_ = s.mutate(func(st *pairState) error {
		for i := range st.Devices {
			if st.Devices[i].ID == id {
				// Throttle: one write a minute is plenty for a "last seen"
				// column and keeps a websocket from rewriting the file.
				if now.Sub(st.Devices[i].LastSeen) < time.Minute {
					return errSkipWrite
				}
				st.Devices[i].LastSeen = now
				return nil
			}
		}
		return errSkipWrite
	})
}

// errSkipWrite aborts a mutation without treating it as a failure.
var errSkipWrite = errors.New("no change")

// revoke ends one device.
func (s *pairStore) revoke(id string) (bool, error) {
	now := s.now().UTC()
	found := false
	err := s.mutate(func(st *pairState) error {
		for i := range st.Devices {
			if st.Devices[i].ID == id && !st.Devices[i].Revoked {
				st.Devices[i].Revoked = true
				st.Devices[i].RevokedAt = now
				found = true
				return nil
			}
		}
		return errSkipWrite
	})
	if errors.Is(err, errSkipWrite) {
		err = nil
	}
	return found, err
}

// revokeAll ends the operator grant, and with it every device and outstanding
// ticket derived from it. One write, no per-device sweep to get wrong.
func (s *pairStore) revokeAll() (int, error) {
	now := s.now().UTC()
	n := 0
	err := s.mutate(func(st *pairState) error {
		for i := range st.Devices {
			if st.deviceLive(st.Devices[i], now) {
				n++
			}
			if !st.Devices[i].Revoked {
				st.Devices[i].Revoked = true
				st.Devices[i].RevokedAt = now
			}
		}
		for i := range st.Tickets {
			if st.Tickets[i].Redeemed == nil {
				st.Tickets[i].Expires = now
			}
		}
		if st.Grant != nil {
			st.Grant.Revoked = true
		}
		return nil
	})
	if errors.Is(err, errSkipWrite) {
		err = nil
	}
	return n, err
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// deviceSubject encodes a device into a session cookie's subject.
func deviceSubject(user, deviceID string) string {
	return user + deviceSubjectSep + deviceID
}

// splitDeviceSubject reverses deviceSubject. ok is false for an ordinary
// operator session, which carries a bare user name.
func splitDeviceSubject(subject string) (user, deviceID string, ok bool) {
	i := strings.Index(subject, deviceSubjectSep)
	if i < 0 {
		return subject, "", false
	}
	return subject[:i], subject[i+len(deviceSubjectSep):], true
}
