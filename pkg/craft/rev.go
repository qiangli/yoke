package craft

// GRAPH REVISION — the state the stores were in when something read them.
//
// `Composition` has carried a `GraphVersion` field since compose.go was
// written, it is threaded into the stamp, and NOTHING HAS EVER COMPUTED IT. So
// every stamp in existence hashes `graph=` with an empty value, and the
// reproducibility receipt is one component short of the claim its own comment
// makes. This file is the producer.
//
// # Why the missing component matters, given that folds are already hashed
//
// The stamp hashes the facts and folds a composition ACTUALLY APPLIED, so a
// changed fold set already changes the stamp. What it cannot see is everything
// the composition did NOT read: a fold at another coordinate, a fact about
// another entity, a skill absorbed an hour ago, an attestation that has since
// accumulated. Two compositions with byte-identical output can therefore come
// from materially different stores — and when a gate verdict lands later and is
// attributed back, "which store produced this" is exactly the question being
// asked. Bytes are addressed by the stamp; the READ is addressed by the rev.
//
// # Sizes, not record counts
//
// The obvious implementation counts records. This one stats. Under the
// append-only, supersede-never-delete discipline every store here already
// follows, a file's SIZE is a monotone exact marker of its state — two
// different contents cannot share a length without a rewrite, and a rewrite is
// a state change that should move the rev anyway. Counting records would mean
// reading every attest ledger on a path that runs at prompt-assembly time,
// which is the one place this repo has measured itself unwilling to spend
// (docs/bashy-startup-performance.md). Stat is O(1) per file.
//
// The empty attest ledger is the one case size alone misses — a new file with
// no records adds zero bytes — so the ledger COUNT is carried alongside.
//
// # Unknown is not zero
//
// A store that cannot be read yields an UNKNOWN rev, not an empty one, and an
// unknown rev renders as the empty string so `GraphVersion` stays absent rather
// than asserting a state nobody observed. That is the same distinction the
// evidence ladder draws between Missing and Unobserved: a boundary we could not
// see past may not be reported as a boundary with nothing behind it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RevVersion prefixes a rendered revision so a stored `graph_version` can be
// recognised, and so the encoding can change without a reader guessing.
const RevVersion = "g1"

// Rev is the observed state of the craft stores, as of one read.
//
// The fields are byte lengths rather than record counts; see the package
// comment. Zero value means UNKNOWN — use Known to tell it from a genuinely
// empty store, because those are different facts.
type Rev struct {
	Folds   int64 `json:"folds"`   // bytes of folds.jsonl
	Facts   int64 `json:"facts"`   // bytes of facts.jsonl
	Attest  int64 `json:"attest"`  // total bytes across attest/*.jsonl
	Ledgers int   `json:"ledgers"` // how many attest ledgers exist
	// Known is false when a store could not be read at all. An unreadable
	// store is not an empty one, and the rev must not claim it was.
	Known bool `json:"known"`
}

// Revision reports the state of the stores under storeDir.
//
// It returns no error, deliberately. This runs on a READ path — compose, and
// later anything that wants to stamp what it saw — and a rev that could not be
// taken is a missing stamp, never a failed composition. The unreadable case is
// carried in the value (Known == false) rather than thrown, so a caller cannot
// accidentally treat "we could not look" as "there was nothing there".
//
// A store that is genuinely absent is a KNOWN empty: a host that has learned
// nothing has a real, reportable state. Only a stat that fails for some other
// reason — permissions, a broken mount — makes the rev unknown.
func Revision(storeDir string) Rev {
	if strings.TrimSpace(storeDir) == "" {
		return Rev{}
	}
	r := Rev{Known: true}

	for _, f := range []struct {
		name string
		into *int64
	}{
		{"folds.jsonl", &r.Folds},
		{"facts.jsonl", &r.Facts},
	} {
		n, ok := sizeOf(filepath.Join(storeDir, f.name))
		if !ok {
			return Rev{}
		}
		*f.into = n
	}

	ents, err := os.ReadDir(filepath.Join(storeDir, "attest"))
	if err != nil {
		if !os.IsNotExist(err) {
			return Rev{}
		}
		return r
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				// Raced with a concurrent writer rotating a ledger. One
				// ledger appearing or vanishing mid-scan is normal on a host
				// several agents share; it is not an unreadable store.
				continue
			}
			return Rev{}
		}
		r.Attest += info.Size()
		r.Ledgers++
	}
	return r
}

func sizeOf(path string) (int64, bool) {
	info, err := os.Stat(path)
	switch {
	case err == nil:
		return info.Size(), true
	case os.IsNotExist(err):
		return 0, true // a store nobody has written to yet is a known empty
	default:
		return 0, false // permissions, a broken mount: we could not look
	}
}

// String renders the revision for the `graph_version` field.
//
// An unknown rev renders EMPTY, so a composition taken against a store we could
// not read carries no GraphVersion at all rather than a placeholder. Downstream
// this means the stamp hashes an empty graph component — exactly today's
// behaviour, which is the correct fallback: the stamp still addresses the bytes,
// it just cannot also address the read.
func (r Rev) String() string {
	if !r.Known {
		return ""
	}
	return fmt.Sprintf("%s:f%d.k%d.a%d/%d", RevVersion, r.Folds, r.Facts, r.Attest, r.Ledgers)
}

// orUnknown renders an absent revision as the word rather than as nothing, so a
// provenance line never reads as `graph=` with a blank that could be mistaken
// for an empty store. Unknown and empty are different facts and the line says
// which one it is.
func orUnknown(rev string) string {
	if rev == "" {
		return "unknown"
	}
	return rev
}

// IsEmpty reports a store that exists and holds nothing. Distinct from unknown:
// callers that need to tell the two apart check Known first.
func (r Rev) IsEmpty() bool {
	return r.Known && r.Folds == 0 && r.Facts == 0 && r.Attest == 0 && r.Ledgers == 0
}
