package craft

// FOLDS — the GENERALISABLE half of what a host learns.
//
// A fact says "on that box the login is svc-build". A fold says "mDNS is
// unreliable on Windows; resolve by IP instead". The second is true on every
// Windows host anyone runs, which is what makes it worth sharing and what makes
// it a different kind of thing.
//
//	FACT   bound to an ENTITY, identity-laden, host-local, never leaves
//	FOLD   bound to a COORDINATE, identity-free, shareable, accretes over time
//
// # The scrubber IS the admission gate, and that is the whole trick
//
// The dangerous mistake is not forgetting to share a fold. It is recording a
// FACT as a fold — "ssh to workshop as svc-build" reads like general advice and
// is nothing of the sort, and once it is in a shareable ring it travels.
//
// Rather than asking authors to classify correctly, the classification is
// CHECKED: a candidate fold goes through pkg/redact, and if the scrubber finds a
// hostname, a username, an address, or a home path, the note is not
// generalisable by definition. It is refused, and the caller is pointed at
// `craft learn`, which is where that knowledge actually belongs.
//
// So the two halves are not a naming convention a reviewer has to police. A
// fold cannot contain identity, because a note containing identity is not
// admitted as one.
//
// # Coordinate, not host
//
// Folds key on the space-time ContextKey — {os, arch} plus the probes a skill
// actually references. That is deliberately NOT a host identifier: two machines
// with the same OS and toolchain share a coordinate, which is exactly why one
// machine's discovery is useful on another. A fold keyed by hostname would help
// nobody but the host that wrote it.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/redact"
)

// Fold is one generalisable thing learned at a coordinate.
type Fold struct {
	// Capability is what the fold amends. Empty means it applies to any skill
	// at this coordinate — an environment truth rather than a skill's quirk.
	Capability string `json:"capability,omitempty"`
	// Coordinate is the space-time context key the fold holds at.
	Coordinate string `json:"coordinate"`
	// Note is what was learned, in words.
	Note string `json:"note"`
	// Evidence is what happened that taught it — the failure, the error. A fold
	// asserted without evidence is an opinion, and opinions should not outlive
	// the session that formed them.
	Evidence string `json:"evidence,omitempty"`

	ObservedAt time.Time  `json:"observed_at"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	Source     string     `json:"source,omitempty"`
}

// Live reports whether the fold is still believed at t.
func (f Fold) Live(t time.Time) bool {
	if f.ValidUntil != nil && !t.Before(*f.ValidUntil) {
		return false
	}
	return !t.Before(f.ValidFrom)
}

// FoldStore is the append-only record of coordinate-keyed knowledge.
//
// Unlike FactStore this one is shareable in principle — it is identity-free by
// construction — so it is not 0600 and it does not hide from export. What keeps
// it safe is the admission gate, not the file mode.
type FoldStore struct {
	path string
	// scrub decides what counts as generalisable. Injectable so tests are
	// hermetic; FromHost in production.
	scrub *redact.Scrubber
}

// OpenFolds opens the fold log under a store directory.
func OpenFolds(storeDir string, scrub *redact.Scrubber) *FoldStore {
	if strings.TrimSpace(storeDir) == "" {
		return &FoldStore{scrub: scrub}
	}
	return &FoldStore{path: filepath.Join(storeDir, "folds.jsonl"), scrub: scrub}
}

// HostScrubber builds the admission scrubber for a store: this machine's own
// identity, PLUS every entity the fact store has learned about.
//
// The second half is not an optimisation, it closes a real hole. redact.FromHost
// knows only the LOCAL hostname and user, so a note naming SOMEONE ELSE'S box
// ("ssh to workshop as svc-build") sails through — the string "workshop" is just
// an English word to a scrubber that has never heard of it. Addresses, emails and
// MACs are caught by shape regardless, but names are not.
//
// The fact store is exactly the missing vocabulary: every `craft learn
// host:workshop …` is a declaration that "workshop" names a machine. Feeding
// those entities back in means the gate gets stricter as the host learns more,
// which is the right direction.
//
// The residual limit is worth stating plainly: a name this host has never
// recorded is still invisible, so the gate is a strong filter rather than a
// proof. It catches every address by shape, this machine's own identity always,
// and any entity previously named — and it cannot catch a hostname nobody here
// has ever mentioned.
func HostScrubber(storeDir string) *redact.Scrubber {
	var opts []redact.Option
	for _, e := range OpenFacts(storeDir).Entities() {
		switch e.Kind {
		case EntityAccount:
			opts = append(opts, redact.WithUser(e.Name))
		default:
			opts = append(opts, redact.WithHost(e.Name))
		}
		// A fact VALUE can be identity too — a learned login is a username
		// wherever it appears, not only under the key that recorded it.
		for _, f := range OpenFacts(storeDir).For(e) {
			switch f.Key {
			case "remote_user", "user", "login", "account":
				opts = append(opts, redact.WithUser(f.Value))
			case "address", "ip", "host", "hostname":
				opts = append(opts, redact.WithHost(f.Value))
			}
		}
	}
	return redact.FromHost(opts...)
}

// Path is the log's location.
func (s *FoldStore) Path() string { return s.path }

// ErrNotGeneralisable reports a candidate fold carrying host identity.
//
// Not a warning: recording it would put a machine's particulars into the one
// store that is meant to be shareable, and the error names the right home for
// it rather than just refusing.
type ErrNotGeneralisable struct {
	Found []redact.Finding
}

func (e *ErrNotGeneralisable) Error() string {
	kinds := make([]string, 0, len(e.Found))
	seen := map[redact.Kind]bool{}
	for _, f := range e.Found {
		if !seen[f.Kind] {
			seen[f.Kind] = true
			kinds = append(kinds, string(f.Kind))
		}
	}
	sort.Strings(kinds)
	return fmt.Sprintf(
		"craft: this is not a fold — it names host identity (%s), so it is true on one machine rather than at a coordinate. "+
			"Record it as a fact instead: `craft learn <entity> <key> <value>`",
		strings.Join(kinds, ", "))
}

// Record appends a fold, after checking that it is actually generalisable.
//
// The check is the point. A note that names a host, a user, or an address is a
// FACT wearing a fold's clothes, and admitting it would put identity into the
// shareable ring — where it travels. Refusing here means the two halves stay
// separate without anyone having to remember which is which.
func (s *FoldStore) Record(f Fold) error {
	if s.path == "" {
		return ErrNoStore
	}
	if strings.TrimSpace(f.Note) == "" {
		return errors.New("craft: fold has no note")
	}
	if strings.TrimSpace(f.Coordinate) == "" {
		return errors.New("craft: fold has no coordinate — a fold that holds nowhere in particular holds nowhere")
	}
	// The NOTE and the EVIDENCE are held to different standards, because they
	// do different jobs.
	//
	// The note is the CLAIM, and it is what a reader acts on. If it names a
	// machine it is not general, and no amount of rewriting makes it so — so a
	// note carrying identity is refused outright.
	//
	// The evidence is PROVENANCE: what the claim rested on, so a doubted fold
	// can be checked rather than argued about. It legitimately names the
	// entities observed, and refusing on that would reject perfectly general
	// claims for citing their sources — which is exactly what happened the
	// first time this ran for real. So evidence is SCRUBBED instead: the tags
	// preserve co-reference (the same entity reads the same everywhere) while
	// carrying no identity, so the count and the shape of the evidence survive
	// and the names do not.
	if s.scrub != nil {
		if _, found := s.scrub.Scrub(f.Note); len(found) > 0 {
			return &ErrNotGeneralisable{Found: found}
		}
		f.Evidence = s.scrub.String(f.Evidence)
	}

	now := time.Now().UTC()
	if f.ObservedAt.IsZero() {
		f.ObservedAt = now
	}
	if f.ValidFrom.IsZero() {
		f.ValidFrom = f.ObservedAt
	}
	return s.append(f)
}

// Retire marks a fold as no longer holding.
//
// Folds go stale the same way facts do — a tool gets fixed, a platform changes —
// and a workaround for a bug that no longer exists is worse than no advice: it
// sends the next agent down a path that is now the slow one.
func (s *FoldStore) Retire(capability, coordinate, note string, at time.Time) error {
	if s.path == "" {
		return ErrNoStore
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	for _, f := range s.All() {
		if f.Capability != capability || f.Coordinate != coordinate || f.Note != note {
			continue
		}
		if !f.Live(at) {
			continue
		}
		f.ValidUntil = &at
		f.ObservedAt = time.Now().UTC()
		return s.append(f)
	}
	return nil
}

func (s *FoldStore) append(f Fold) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(f)
}

// All returns every record, oldest first.
func (s *FoldStore) All() []Fold {
	if s.path == "" {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil // never written to is empty, not broken
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []Fold
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var f Fold
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue
		}
		out = append(out, f)
	}
	return out
}

// For returns the folds that hold for a capability at a coordinate.
//
// Folds with an empty Capability are environment truths and apply to every
// skill at the coordinate, so they come back too. A fold recorded at a DIFFERENT
// coordinate is not returned: that is the whole point of keying on one.
func (s *FoldStore) For(capability, coordinate string) []Fold {
	now := time.Now().UTC()
	retired := map[string]bool{}
	live := map[string]Fold{}
	for _, f := range s.All() {
		if f.Coordinate != coordinate {
			continue
		}
		if f.Capability != "" && f.Capability != capability {
			continue
		}
		k := f.Capability + "\x00" + f.Note
		if !f.Live(now) {
			retired[k] = true
			delete(live, k)
			continue
		}
		if retired[k] {
			// A later record reopens a retired fold; the log is replayed in
			// order and the last word wins.
			retired[k] = false
		}
		live[k] = f
	}
	out := make([]Fold, 0, len(live))
	for _, f := range live {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Note < out[j].Note })
	return out
}

// Coordinates lists every coordinate with at least one live fold.
func (s *FoldStore) Coordinates() []string {
	now := time.Now().UTC()
	seen := map[string]bool{}
	var out []string
	for _, f := range s.All() {
		if f.Live(now) && !seen[f.Coordinate] {
			seen[f.Coordinate] = true
			out = append(out, f.Coordinate)
		}
	}
	sort.Strings(out)
	return out
}
