// Lexical memory retrieval, Wave2 of story a566e6387674.
//
// Retrieve is the one shared read pipeline for scoped memory retrieval. It
// composes the db library directly (contract §10.1) beside pkg/recall: kb
// recall, kb search and 284 M4 seed from here. It reads exactly two explicit
// targets and never creates, writes or provisions anything:
//
//   - the memory document target (sqlite file/mem, collection "memory") is
//     the source of truth for records, revisions and deletions;
//   - the full-text index target (bleve file/mem) is the lexical rank
//     source. It holds one indexed source document per memory record under
//     the same stable record ID.
//
// Populating the index is not a read: the capture side owns no FTS target
// or config, so this story provides the explicit SyncIndex verb (op
// memory.index.sync) that builds and rebuilds the index from a bounded
// consistent source snapshot. Recall itself never writes: a missing
// collection or index target is not_found naming that target, never an
// implicit build.
//
// Both verbs run their index side as registered category operations on the
// index target, so the enumerate-then-act sequence executes inside ONE
// admitted handle hold instead of separate owner calls:
//
//   - memory.index.sync (AccessWrite) takes the bounded source snapshot as
//     input and, on the same handle, reads the bounded index snapshot,
//     diffs, and applies one atomic fts.batch. Owner admission serializes
//     writers sharing a session; a racing direct-path writer loses
//     atomically on the batch IfRevision/delete guards and converges on
//     the next sync, never as a half-applied batch.
//   - memory.retrieve (AccessRead) brackets the bounded index snapshot
//     and the bounded ranked query hits with two dbfts.Generation reads
//     on the same handle: a writer committing mid-read moves the counter
//     (every batch bumps it, content ABA included) and the read returns
//     busy with no output instead of mixing generations. It sorts by
//     score descending (stable by record ID) before the requested limit,
//     and reports the index fingerprint. The source snapshot beside it is
//     a separate explicit point: no cross-target atomicity is claimed.
//
// A served hit's ref is the stable record ID verbatim; the canonical
// source target travels separately in the freshness report and the
// envelope target, so a record is named by the tuple
// (target, collection, ID) with no invented locator syntax. Lifecycle is
// resolved at consumption through the shared kblife semantics:
// kblife.Visible(record.State(asOf)), defaulting to one fixed now per
// retrieval with an optional --as-of for historical reads.
//
// Semantic and hybrid modes rank through an optional sqlite vector target
// (retrieval_semantic.go): the query is embedded through the existing local
// model door, the bounded vector population is ranked through the vec
// category on the shared admission path, and hybrid fuses both lists with
// reciprocal rank fusion. Without an explicitly configured vector target
// and embedding model they return explicit unsupported, never a silent
// substitution — unless the caller chose the labeled lexical fallback.
package kbmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dbfts"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/kblife"
)

const (
	// RetrievalSchema versions successful retrieval envelopes. Errors reuse
	// ErrorSchema unchanged: no new error, cursor or locator ABI.
	RetrievalSchema = "bashy-kb-memory-retrieval-v1"
	// RetrievalOp is the envelope op for every retrieval call.
	RetrievalOp = "memory.retrieve"
	// IndexEngine is the only engine with the full-text category today.
	IndexEngine = "bleve"
	// Retrieval modes. Only lexical is served; the others fail explicitly.
	ModeLexical  = "lexical"
	ModeSemantic = "semantic"
	ModeHybrid   = "hybrid"
)

// RetrieveQuery is one typed retrieval operation. CLI and MCP both build it
// from the same flags. Empty Text matches every indexed record (a pure
// filter browse). Tags all must match; Repos matches on any overlap; OS is
// exact; Since is inclusive and Until exclusive on the record's created
// stamp. Zero Limit/MaxBytes take the category defaults.
//
// Semantic and hybrid modes additionally read Vector, an optional S11 db
// locator for the sqlite vector rank target (never provisioned here), with
// VectorCollection naming its vec collection (default "memory") and
// EmbedModel naming the explicit local embedding model, which must match
// the collection's persisted model. Embed injects the embedding entry
// point (nil selects the local model door); FallbackLexical serves labeled
// lexical results instead of failing when a vector mode has no backend.
type RetrieveQuery struct {
	Mode      string
	Target    string // S11 db locator for the sqlite memory collection
	Index     string // S11 db locator for the bleve rank index
	Session   string // explicit owner session for a persistent target
	Dir       string // resolves a relative persistent path
	Text      string
	Tags      []string
	Repos     []string
	OS        string
	Since     string
	Until     string
	Limit     int
	MaxBytes  int
	AsOf      string // optional RFC 3339 instant for lifecycle visibility; empty means one fixed now per retrieval
	RequestID string
	Wait      time.Duration
	// Vector is the optional S11 db locator for the sqlite vector rank
	// target behind semantic and hybrid modes.
	Vector string
	// VectorCollection names the vec collection holding memory embeddings
	// (default DefaultVectorCollection).
	VectorCollection string
	// EmbedModel is the explicit local embedding model for semantic and
	// hybrid modes; it must match the vector collection's persisted model.
	EmbedModel string
	// FallbackLexical serves labeled lexical results (Result.Fallback)
	// instead of failing when a vector mode has no backend or embedding
	// fails.
	FallbackLexical bool
	// Embed injects the embedding entry point; nil selects EmbedLocal,
	// the existing local model door.
	Embed Embedder
}

// Hit is one served record: Ref is the stable record ID verbatim, beside
// the lexical score, the mode actually used, and the source-of-truth
// revision. The canonical source target is not folded into the ref: it
// travels in the freshness report and the envelope target, so a record is
// named by the tuple (target, collection, ID). Stale marks a hit whose
// indexed bytes differed from the served document.
type Hit struct {
	Ref           string   `json:"ref"`
	Score         float64  `json:"score"`
	Mode          string   `json:"mode"`
	Revision      int64    `json:"revision"`
	IndexRevision string   `json:"index_revision,omitempty"`
	Stale         bool     `json:"stale,omitempty"`
	Record        Record   `json:"record"`
	Why           []string `json:"why,omitempty"`
}

// Freshness reports what the call observed about its two targets: the index
// population it ranked, how many hits it examined, and how many indexed
// hits named records the collection no longer holds. SourceDocuments and
// IndexDocuments are the bounded-population sizes, but staleness never
// rests on counts: SourceRevision and IndexRevision are sha256 fingerprints
// over the bounded source and index snapshots (decoded memory records in
// stable form, so a re-encoding is not a change), and Stale is true exactly
// when they differ. A same-count edit outside the query and a same-count
// delete-plus-add both show stale even though no count moves.
//
// Vector modes additionally report the vector rank target they read
// (VectorTarget with its persisted VectorModel, VectorDimension and live
// VectorDocuments count). The vector side has no cheap population snapshot,
// so its IndexRevision is the collection identity label and Stale also
// turns true when examined candidates show indexed bytes that differ from
// the served documents or any candidate is dropped.
type Freshness struct {
	DocTarget       string `json:"doc_target"`
	IndexTarget     string `json:"index_target"`
	SourceDocuments int64  `json:"source_documents"`
	IndexDocuments  uint64 `json:"index_documents"`
	SourceRevision  string `json:"source_revision"`
	IndexRevision   string `json:"index_revision"`
	Stale           bool   `json:"stale,omitempty"`
	IndexTotal      uint64 `json:"index_total"`
	Examined        int    `json:"examined"`
	DroppedStale    int    `json:"dropped_stale"`
	DroppedState    int    `json:"dropped_state,omitempty"`
	Truncated       bool   `json:"truncated"`
	// VectorTarget is the vector rank target behind semantic and hybrid
	// modes, with the persisted embedding identity it was validated
	// against through the vec category.
	VectorTarget    string `json:"vector_target,omitempty"`
	VectorModel     string `json:"vector_model,omitempty"`
	VectorDimension int    `json:"vector_dimension,omitempty"`
	VectorDocuments int64  `json:"vector_documents,omitempty"`
}

// RetrieveResult is the result body of a successful retrieval envelope.
// Fallback is true when the caller explicitly chose --fallback-lexical and
// a vector mode was served as labeled lexical results.
type RetrieveResult struct {
	Hits      []Hit     `json:"hits"`
	Freshness Freshness `json:"freshness"`
	Mode      string    `json:"mode"`
	Fallback  bool      `json:"fallback,omitempty"`
}

// Retrieve runs one scoped lexical retrieval and returns its envelope. It
// never panics on caller input and never creates a target or an index.
func Retrieve(ctx context.Context, q RetrieveQuery) Envelope {
	env := Envelope{SchemaVersion: RetrievalSchema, OK: true, Op: RetrievalOp,
		Collection: Collection, MutationClass: "read", RequestID: q.RequestID}
	res, err := retrieve(ctx, q, &env)
	if err != nil {
		return fail(env, err)
	}
	env.Result = res
	return env
}

func retrieve(ctx context.Context, q RetrieveQuery, env *Envelope) (RetrieveResult, error) {
	var out RetrieveResult
	mode, fallback, err := resolveMode(q)
	if err != nil {
		return out, err
	}
	limit, maxBytes, err := retrieveBounds(q.Limit, q.MaxBytes)
	if err != nil {
		return out, err
	}
	since, until, wantSince, wantUntil, err := retrieveWindow(q.Since, q.Until)
	if err != nil {
		return out, err
	}
	asOf, err := retrieveAsOf(q.AsOf)
	if err != nil {
		return out, err
	}
	docT, idxT, err := parseRetrieveTargets(q)
	if err != nil {
		return out, err
	}
	docID, err := dbstore.Resolve(docT)
	if err != nil {
		return out, err
	}
	if _, err := dbstore.Resolve(idxT); err != nil {
		return out, err
	}
	info := dblifecycle.ViewTarget(docID, "")
	env.Target = &info
	f := retrieveFilter{tags: clean(q.Tags), repos: clean(q.Repos), os: strings.TrimSpace(q.OS),
		since: since, until: until, wantSince: wantSince, wantUntil: wantUntil}
	if mode != ModeLexical {
		return runVectorRetrieve(ctx, q, mode, docT, idxT, docID, f, limit, maxBytes, asOf)
	}
	res, err := runRetrieve(ctx, q, docT, idxT, docID, f, limit, maxBytes, asOf)
	if err != nil {
		return out, err
	}
	res.Fallback = fallback
	return res, nil
}

// retrieveAsOf resolves the lifecycle instant for one retrieval: one fixed
// now by default, or the caller's explicit RFC 3339 instant for a
// historical read through the same shared kblife semantics.
func retrieveAsOf(asOf string) (time.Time, error) {
	if strings.TrimSpace(asOf) == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(asOf))
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: --as-of %q is not RFC 3339", dbstore.ErrInvalidInput, asOf)
	}
	return t, nil
}

// checkRetrieveMode admits every known mode name. Backend readiness for
// the vector modes is decided per query by resolveMode in
// retrieval_semantic.go: without a vector target and an embedding entry
// point they stay structured unsupported unless the caller chose the
// labeled lexical fallback.
func checkRetrieveMode(mode string) error {
	switch strings.TrimSpace(mode) {
	case "", ModeLexical, ModeSemantic, ModeHybrid:
		return nil
	default:
		return fmt.Errorf("%w: mode %q (want lexical|semantic|hybrid)", dbstore.ErrInvalidInput, mode)
	}
}

func retrieveBounds(limit, maxBytes int) (int, int, error) {
	if limit == 0 {
		limit = dbfts.DefaultLimit
	}
	if maxBytes == 0 {
		maxBytes = dbfts.DefaultMaxBytes
	}
	if limit < 0 || limit > dbfts.MaxLimit || maxBytes <= 0 || maxBytes > dbfts.MaxMaxBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, dbfts.MaxLimit, dbfts.MaxMaxBytes)
	}
	return limit, maxBytes, nil
}

func retrieveWindow(since, until string) (time.Time, time.Time, bool, bool, error) {
	var s, u time.Time
	var ws, wu bool
	if strings.TrimSpace(since) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(since))
		if err != nil {
			return s, u, false, false, fmt.Errorf("%w: --since %q is not RFC 3339", dbstore.ErrInvalidInput, since)
		}
		s, ws = t, true
	}
	if strings.TrimSpace(until) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(until))
		if err != nil {
			return s, u, false, false, fmt.Errorf("%w: --until %q is not RFC 3339", dbstore.ErrInvalidInput, until)
		}
		u, wu = t, true
	}
	if ws && wu && !s.Before(u) {
		return s, u, false, false, fmt.Errorf("%w: --since must be before --until", dbstore.ErrInvalidInput)
	}
	return s, u, ws, wu, nil
}

// parseRetrieveTargets resolves the two explicit read targets. Either one
// may be absent from disk; the read then fails with not_found naming it.
// Nothing here creates, and a relative persistent path resolves under Dir.
func parseRetrieveTargets(q RetrieveQuery) (dbstore.Target, dbstore.Target, error) {
	if strings.TrimSpace(q.Target) == "" {
		return dbstore.Target{}, dbstore.Target{}, fmt.Errorf("%w: an explicit TARGET locator is required (the db locator, e.g. db:sqlite%%3Afile%%3APATH)", dbstore.ErrInvalidInput)
	}
	if strings.TrimSpace(q.Index) == "" {
		return dbstore.Target{}, dbstore.Target{}, fmt.Errorf("%w: an explicit --index FTS locator is required (retrieval never provisions one, e.g. db:bleve%%3Afile%%3APATH)", dbstore.ErrInvalidInput)
	}
	docT, err := dblifecycle.ParseTarget(q.Target)
	if err != nil {
		return dbstore.Target{}, dbstore.Target{}, err
	}
	idxT, err := dblifecycle.ParseTarget(q.Index)
	if err != nil {
		return dbstore.Target{}, dbstore.Target{}, err
	}
	if docT.Engine != Engine {
		return dbstore.Target{}, dbstore.Target{}, fmt.Errorf("%w: memory retrieval reads the document category (engine %s), not %s", dbstore.ErrUnsupported, Engine, docT.Engine)
	}
	if idxT.Engine != IndexEngine {
		return dbstore.Target{}, dbstore.Target{}, fmt.Errorf("%w: memory retrieval ranks on the full-text category (engine %s), not %s", dbstore.ErrUnsupported, IndexEngine, idxT.Engine)
	}
	for _, tc := range []struct {
		t    *dbstore.Target
		what string
	}{
		{&docT, "TARGET"}, {&idxT, "--index"},
	} {
		if tc.t.Mode == dbstore.Memory && q.Session != "" && q.Session != tc.t.Session {
			return dbstore.Target{}, dbstore.Target{}, fmt.Errorf("%w: --session %s does not own %s session %s", dbstore.ErrInvalidInput, q.Session, tc.what, tc.t.Session)
		}
		if tc.t.Mode == dbstore.Persistent && !filepath.IsAbs(tc.t.Path) && q.Dir != "" {
			tc.t.Path = filepath.Join(q.Dir, tc.t.Path)
		}
	}
	return docT, idxT, nil
}

// retrieveSession is the owner a read goes through: the target's own
// session for a memory target, or an explicitly given shared owner for a
// persistent target.
func retrieveSession(q RetrieveQuery, t dbstore.Target) string {
	if t.Mode == dbstore.Memory {
		return t.Session
	}
	return q.Session
}

// catOp runs one read-only category operation through the shared admission
// path: the owner session when one owns the target, the direct engine path
// otherwise. It never creates.
func catOp(ctx context.Context, q RetrieveQuery, t dbstore.Target, op string, in any) (dblifecycle.Outcome, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if s := retrieveSession(q, t); s != "" {
		res, err := dblifecycle.Call(ctx, s, dblifecycle.Request{Op: op, Target: t, Wait: q.Wait, RequestID: q.RequestID, Input: raw})
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if !res.OK {
			if res.Error == nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: owner refused %s without an error", dbstore.ErrBackend, op)
			}
			return dblifecycle.Outcome{}, &ownerError{*res.Error}
		}
		return dblifecycle.Outcome{Result: res.Result, Page: res.Page, Warnings: res.Warnings}, nil
	}
	if q.RequestID != "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: --request-id deduplication is an owner-session guarantee; pass --session", dbstore.ErrUnsupported)
	}
	_, out, err := dblifecycle.Service{}.RunCategory(ctx, t, op, raw, false, q.Wait)
	return out, err
}

// This story's index side runs as registered category operations on the
// index target, so each enumerate-then-act sequence holds ONE admission
// across the whole custom op — the owner serializes writers sharing a
// session around it, and the direct path holds the claim-guarded handle
// open across it. Nothing here adds a lock registry: the guard is the
// existing admission both paths already provide.
func init() {
	dblifecycle.RegisterCategoryOp(RetrievalOp, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			if _, err := decodeRetrieveOp(raw); err != nil {
				return 0, err
			}
			return dblifecycle.AccessRead, nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
			return retrieveSpec(mode)
		},
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := decodeRetrieveOp(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			return runRetrieveOp(ctx, h, in)
		},
	})
	dblifecycle.RegisterCategoryOp(SyncOp, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			if _, err := decodeSyncOp(raw); err != nil {
				return 0, err
			}
			return dblifecycle.AccessWrite, nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
			return syncSpec(mode)
		},
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := decodeSyncOp(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			return runSyncOp(ctx, h, in)
		},
	})
}

func retrieveSpec(mode dbstore.Mode) dblifecycle.Operation {
	s := dblifecycle.Operation{Name: RetrievalOp, MutationClass: "read",
		Hold: "none", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	if mode == dbstore.Memory {
		s.Effects = []string{"db.read"}
	} else {
		s.Effects = []string{"fs.read", "db.read"}
	}
	return s
}

func syncSpec(mode dbstore.Mode) dblifecycle.Operation {
	s := dblifecycle.Operation{Name: SyncOp, MutationClass: "write",
		Hold: "shared-writer", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	if mode == dbstore.Memory {
		s.Effects = []string{"db.write"}
	} else {
		s.Effects = []string{"fs.write", "db.write"}
	}
	return s
}

// retrieveOpInput is the typed input of the memory.retrieve category op:
// the rank text plus the caller's requested hit bound. The population caps
// stay the story caps (MaxSyncRecords/MaxSyncBytes): the op queries at the
// max bounded population and sorts by score before the requested limit,
// because dbfts orders _id and its first N is not the best N.
type retrieveOpInput struct {
	Query string `json:"query,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func decodeRetrieveOp(raw json.RawMessage) (retrieveOpInput, error) {
	var in retrieveOpInput
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return in, fmt.Errorf("%w: memory.retrieve input: %v", dbstore.ErrInvalidInput, err)
		}
	}
	if in.Limit < 0 || in.Limit > MaxSyncRecords {
		return in, fmt.Errorf("%w: memory.retrieve limit 0..%d", dbstore.ErrInvalidInput, MaxSyncRecords)
	}
	return in, nil
}

// retrieveOpOutput is the typed result of one admitted index read: the
// bounded index snapshot fingerprint beside the bounded ranked hits, both
// taken on the same handle.
type retrieveOpOutput struct {
	IndexRevision  string   `json:"index_revision"`
	IndexDocuments uint64   `json:"index_documents"`
	Total          uint64   `json:"total"`
	Hits           []ftsHit `json:"hits"`
	Truncated      bool     `json:"truncated"`
}

// ftsOnHandle runs one already-registered fts category op on the admitted
// handle this op holds, reusing its implementation (bounds, guards, byte
// packing, generation checks) without a second admission. It enters no
// gate of its own: the write op it may call enters the fts writer gate
// once, exactly as a direct fts.batch would.
func ftsOnHandle(ctx context.Context, h dbstore.Handle, name string, in dbfts.Input) (dblifecycle.Outcome, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	op, access, ok, err := dblifecycle.LookupCategoryOp(name, raw)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !ok {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: db operation %s", dbstore.ErrUnsupported, name)
	}
	return op.Run(ctx, h, raw, access)
}

// searchHandle runs one ranked index read on the admitted handle and
// decodes its page.
func searchHandle(ctx context.Context, h dbstore.Handle, in dbfts.Input) (ftsPage, string, error) {
	var page ftsPage
	out, err := ftsOnHandle(ctx, h, dbfts.OpSearch, in)
	if err != nil {
		return page, "", err
	}
	if err := decode(out.Result, &page); err != nil {
		return page, "", fmt.Errorf("%w: index result: %v", dbstore.ErrIntegrity, err)
	}
	var paging struct {
		Next string `json:"next"`
	}
	if out.Page != nil {
		if err := decode(out.Page, &paging); err != nil {
			return page, "", fmt.Errorf("%w: index page: %v", dbstore.ErrIntegrity, err)
		}
	}
	return page, paging.Next, nil
}

// snapshotHandle reads the whole bounded index population on the admitted
// handle: one match-all fts.search. Any next cursor refuses with
// limit_exceeded, so neither verb reasons from half a population. The
// per-entry revisions feed freshness and the sync batch IfRevision guards.
func snapshotHandle(ctx context.Context, h dbstore.Handle) (map[string]indexDoc, error) {
	page, next, err := searchHandle(ctx, h,
		dbfts.Input{Limit: MaxSyncRecords, MaxBytes: MaxSyncBytes})
	if err != nil {
		return nil, err
	}
	if next != "" {
		return nil, fmt.Errorf("%w: full-text index holds %d entries, over the snapshot cap (%d records, %d bytes); refusing a partial snapshot",
			dbstore.ErrLimitExceeded, page.Total, MaxSyncRecords, MaxSyncBytes)
	}
	got := make(map[string]indexDoc, len(page.Hits))
	for _, h := range page.Hits {
		if _, dup := got[h.ID]; dup {
			return nil, fmt.Errorf("%w: index holds %q twice", dbstore.ErrIntegrity, h.ID)
		}
		got[h.ID] = indexDoc{revision: h.Revision, raw: h.Doc}
	}
	return got, nil
}

// retrieveProbe is a test hook run between the index snapshot and the
// ranked query reads: tests advance the index there to prove the
// generation guard below returns busy instead of mixing generations.
var retrieveProbe func(context.Context, dbstore.Handle)

// runRetrieveOp reads the bounded index snapshot (match-all, for the
// fingerprint) and the bounded ranked query hits (for serving) on the one
// admitted handle it was given, then sorts by score descending (stable by
// record ID) before the requested limit — dbfts orders _id, so its first N
// is not the best N. An empty query browses: the ranked hits are the
// snapshot entries in stable ID order. A ranked side past the packing bound
// serves best-effort with truncated set: a foreign oversized population
// still ranks rather than failing the read.
//
// A read stays AccessRead: the owner serializes writers around it but does
// not exclude them, so the op brackets both reads with dbfts.Generation on
// the same handle. A writer committing between the snapshot and the query
// moves the batch counter (content ABA included) and the op returns busy
// with no output instead of a mixed-generation claim.
func runRetrieveOp(ctx context.Context, h dbstore.Handle, in retrieveOpInput) (dblifecycle.Outcome, error) {
	before, err := dbfts.Generation(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	snap, err := snapshotHandle(ctx, h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if retrieveProbe != nil {
		retrieveProbe(ctx, h)
	}
	out := retrieveOpOutput{
		IndexRevision:  fingerprintIndex(snap),
		IndexDocuments: uint64(len(snap)),
	}
	if strings.TrimSpace(in.Query) == "" {
		for id, d := range snap {
			out.Hits = append(out.Hits, ftsHit{ID: id, Revision: d.revision, Doc: d.raw})
		}
		slices.SortFunc(out.Hits, func(a, b ftsHit) int { return strings.Compare(a.ID, b.ID) })
		out.Total = uint64(len(out.Hits))
	} else {
		page, next, err := searchHandle(ctx, h,
			dbfts.Input{Query: in.Query, Limit: MaxSyncRecords, MaxBytes: MaxSyncBytes})
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		out.Total, out.Truncated = page.Total, next != ""
		out.Hits = page.Hits
		slices.SortStableFunc(out.Hits, func(a, b ftsHit) int {
			if a.Score != b.Score {
				if a.Score > b.Score {
					return -1
				}
				return 1
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	after, err := dbfts.Generation(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if after != before {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: full-text index changed during the read; retry the recall", dbstore.ErrBusy)
	}
	return dblifecycle.Outcome{Result: out}, nil
}

// syncOpDoc is one source record of the bounded source snapshot the sync
// op reconciles.
type syncOpDoc struct {
	ID  string          `json:"id"`
	Doc json.RawMessage `json:"doc"`
}

// syncOpInput is the typed input of the memory.index.sync category op: the
// bounded consistent source snapshot, taken by the caller as ONE doc.find
// read transaction before this admission. The op never reads the document
// target itself: one handle, one target.
type syncOpInput struct {
	Collection string      `json:"collection"`
	Documents  []syncOpDoc `json:"documents"`
}

func decodeSyncOp(raw json.RawMessage) (syncOpInput, error) {
	var in syncOpInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, fmt.Errorf("%w: memory.index.sync input: %v", dbstore.ErrInvalidInput, err)
	}
	if in.Collection != Collection {
		return in, fmt.Errorf("%w: memory.index.sync reconciles collection %q, not %q", dbstore.ErrInvalidInput, Collection, in.Collection)
	}
	if len(in.Documents) > MaxSyncRecords {
		return in, fmt.Errorf("%w: sync snapshot holds %d records, over the atomic batch bound %d",
			dbstore.ErrLimitExceeded, len(in.Documents), MaxSyncRecords)
	}
	return in, nil
}

// runSyncOp reconciles the admitted index with the source snapshot on the
// same handle: one bounded index snapshot, one diff, one atomic fts.batch
// for the adds, updates and deletes the snapshots imply. A snapshot past
// the caps, or more operations than one batch holds, fails with
// limit_exceeded before anything is written. Updates carry the indexed
// IfRevision, so a concurrent index write to the same entry fails with
// conflict instead of mixing silently; deletes are enumerated from the
// index snapshot and applied in the same atomic batch. Like init, a sync is
// not one replayable owner request, so it takes no request ID.
func runSyncOp(ctx context.Context, h dbstore.Handle, in syncOpInput) (dblifecycle.Outcome, error) {
	snap := make(map[string]json.RawMessage, len(in.Documents))
	for _, d := range in.Documents {
		if err := ctx.Err(); err != nil {
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		var rec Record
		if err := json.Unmarshal(d.Doc, &rec); err != nil || rec.ID == "" || rec.ID != d.ID {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: snapshot record %q is not a memory record", dbstore.ErrIntegrity, d.ID)
		}
		if _, dup := snap[d.ID]; dup {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: snapshot holds %q twice", dbstore.ErrIntegrity, d.ID)
		}
		snap[d.ID] = d.Doc
	}
	indexed, err := snapshotHandle(ctx, h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	var puts []dbfts.DocumentEntry
	var deletes []string
	for id, raw := range snap {
		old, ok := indexed[id]
		if !ok {
			puts = append(puts, dbfts.DocumentEntry{ID: id, Doc: raw})
			continue
		}
		if !sameRecord(old, raw) {
			puts = append(puts, dbfts.DocumentEntry{ID: id, Doc: raw, IfRevision: old.revision})
		}
	}
	for id := range indexed {
		if _, ok := snap[id]; !ok {
			deletes = append(deletes, id)
		}
	}
	slices.Sort(deletes)
	res := SyncResult{SourceDocuments: len(snap),
		Added: countNew(puts, indexed), Updated: len(puts) - countNew(puts, indexed), Deleted: len(deletes)}
	if len(puts)+len(deletes) == 0 {
		res.IndexDocuments = uint64(len(indexed))
		return dblifecycle.Outcome{Result: res}, nil
	}
	if len(puts)+len(deletes) > dbfts.MaxLimit {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: sync needs %d index operations, over the atomic batch bound %d; narrow the collection",
			dbstore.ErrLimitExceeded, len(puts)+len(deletes), dbfts.MaxLimit)
	}
	if _, err := ftsOnHandle(ctx, h, dbfts.OpBatch,
		dbfts.Input{Documents: puts, Deletes: deletes, Replace: true}); err != nil {
		return dblifecycle.Outcome{}, err
	}
	// The batch reports puts as updated, so the net index size moves by new
	// puts minus deletes.
	res.IndexDocuments = uint64(len(indexed) + res.Added - res.Deleted)
	return dblifecycle.Outcome{Result: res}, nil
}

// retrieveFilter is the conjunctive post-rank filter over fresh records.
type retrieveFilter struct {
	tags                 []string
	repos                []string
	os                   string
	since, until         time.Time
	wantSince, wantUntil bool
}

// why names the query clauses that selected a hit, in stable order.
func (f retrieveFilter) why(text string) []string {
	why := []string{}
	if strings.TrimSpace(text) != "" {
		why = append(why, "text")
	}
	if len(f.tags) > 0 {
		why = append(why, "tags")
	}
	if len(f.repos) > 0 || f.os != "" {
		why = append(why, "scope")
	}
	if f.wantSince || f.wantUntil {
		why = append(why, "time")
	}
	return why
}

// match reports whether a fresh record passes every requested clause.
func (f retrieveFilter) match(r Record) (bool, error) {
	for _, want := range f.tags {
		if !slices.Contains(r.Tags, want) {
			return false, nil
		}
	}
	if len(f.repos) > 0 {
		hit := false
		for _, repo := range f.repos {
			if slices.Contains(r.Scope.Repos, repo) {
				hit = true
				break
			}
		}
		if !hit {
			return false, nil
		}
	}
	if f.os != "" && r.Scope.OS != f.os {
		return false, nil
	}
	if f.wantSince || f.wantUntil {
		at, err := time.Parse(time.RFC3339Nano, r.Created)
		if err != nil || r.Created == "" {
			return false, fmt.Errorf("%w: memory record %q has no parseable created stamp", dbstore.ErrIntegrity, r.ID)
		}
		if f.wantSince && at.Before(f.since) {
			return false, nil
		}
		if f.wantUntil && !at.Before(f.until) {
			return false, nil
		}
	}
	return true, nil
}

// ftsHit is one ranked index entry with its stored source bytes.
type ftsHit struct {
	ID       string          `json:"id"`
	Revision string          `json:"revision"`
	Score    float64         `json:"score"`
	Doc      json.RawMessage `json:"doc"`
}

type ftsPage struct {
	Total uint64   `json:"total"`
	Hits  []ftsHit `json:"hits"`
}

func runRetrieve(ctx context.Context, q RetrieveQuery, docT, idxT dbstore.Target, docID dbstore.Identity, f retrieveFilter, limit, maxBytes int, asOf time.Time) (RetrieveResult, error) {
	out := RetrieveResult{Mode: ModeLexical, Hits: []Hit{}}
	out.Freshness.DocTarget = docID.ID
	if id, err := dbstore.Resolve(idxT); err == nil {
		out.Freshness.IndexTarget = id.ID
	}
	// One bounded source snapshot (a single doc.find read transaction)
	// beside one admitted index read (snapshot plus ranked hits on one
	// handle). The source side is a separate explicit point — no
	// cross-target atomicity is claimed — and every hit below resolves
	// against the same source snapshot instead of per-hit reads. Recall
	// still writes nothing.
	srcSnap, err := snapshotSource(ctx, q, docT, MaxSyncRecords, MaxSyncBytes)
	if err != nil {
		return out, err
	}
	idxOut, err := catOp(ctx, q, idxT, RetrievalOp, retrieveOpInput{Query: q.Text, Limit: limit})
	if err != nil {
		return out, err
	}
	var rop retrieveOpOutput
	{
		b, err := json.Marshal(idxOut.Result)
		if err != nil {
			return out, err
		}
		if err := json.Unmarshal(b, &rop); err != nil {
			return out, fmt.Errorf("%w: index read result: %v", dbstore.ErrIntegrity, err)
		}
	}
	out.Freshness.SourceDocuments = int64(len(srcSnap))
	out.Freshness.IndexDocuments = rop.IndexDocuments
	out.Freshness.SourceRevision = fingerprintSource(srcSnap)
	out.Freshness.IndexRevision = rop.IndexRevision
	out.Freshness.Stale = out.Freshness.SourceRevision != out.Freshness.IndexRevision
	out.Freshness.IndexTotal = rop.Total
	out.Freshness.Truncated = rop.Truncated
	cands := make([]rankCandidate, 0, len(rop.Hits))
	for _, h := range rop.Hits {
		cands = append(cands, rankCandidate{id: h.ID, rev: h.Revision, score: h.Score, doc: h.Doc})
	}
	if err := serveRanked(ctx, srcSnap, cands, false, true, f, f.why(q.Text), ModeLexical, limit, maxBytes, asOf, &out); err != nil {
		return out, err
	}
	return out, nil
}

type servedHit struct {
	hit   Hit
	bytes int
}

// liveRecord resolves lifecycle visibility at consumption through the one
// shared semantics (nothing copied here): kblife.Visible over the record's
// own State at the retrieval's as-of instant. A forgotten, superseded,
// expired or not-yet-valid record stays stored but is never served; a
// historical as-of can still show a then-valid interval.
func liveRecord(r Record, asOf time.Time) bool {
	return kblife.Visible(r.State(asOf))
}

// serveSnapHit resolves one ranked index entry against the source snapshot
// the call already holds and applies the post-rank filter. A hit whose
// document is gone from the snapshot is dropped, never served: the source
// of truth deleted it and the index has not caught up. A hit whose fresh
// record left the live lifecycle at the retrieval's as-of is dropped the
// same way. stop reports the byte budget is spent and the call must end
// truncated.
func serveSnapHit(snap map[string]sourceDoc, h ftsHit, f retrieveFilter, why []string, used, maxBytes int, asOf time.Time, mode string) (*servedHit, bool, error) {
	d, ok := snap[h.ID]
	if !ok {
		return nil, false, errDropped
	}
	var fresh Record
	if err := json.Unmarshal(d.raw, &fresh); err != nil || fresh.ID == "" {
		return nil, false, fmt.Errorf("%w: memory record %q: %v", dbstore.ErrIntegrity, h.ID, err)
	}
	if !liveRecord(fresh, asOf) {
		return nil, false, errDroppedState
	}
	var indexed Record
	if err := json.Unmarshal(h.Doc, &indexed); err != nil || indexed.ID == "" {
		return nil, false, fmt.Errorf("%w: index holds an undecodable source for %q", dbstore.ErrIntegrity, h.ID)
	}
	match, err := f.match(fresh)
	if err != nil {
		return nil, false, err
	}
	if !match {
		return nil, false, nil
	}
	size := len(d.raw)
	if used+size > maxBytes {
		if used == 0 {
			return nil, false, fmt.Errorf("%w: memory record %q is %d bytes, over --max-bytes %d", dbstore.ErrLimitExceeded, h.ID, size, maxBytes)
		}
		return nil, true, nil
	}
	return &servedHit{hit: Hit{Ref: fresh.ID, Score: h.Score, Mode: mode,
		Revision: d.revision, IndexRevision: h.Revision, Stale: !reflect.DeepEqual(indexed, fresh),
		Record: fresh, Why: why}, bytes: size}, false, nil
}

// fingerprintRecords digests one bounded population snapshot: the sorted
// record IDs with their decoded records in stable form. Decoding (not raw
// bytes) is hashed so an export re-encoding of identical fields is not a
// change — the same semantic comparison SyncIndex uses. An undecodable
// entry still contributes (as raw bytes), so corruption reads stale rather
// than matching a healthy peer.
func fingerprintRecords(raws map[string]json.RawMessage) string {
	ids := make([]string, 0, len(raws))
	for id := range raws {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	h := sha256.New()
	for _, id := range ids {
		var r Record
		if canon, err := canonicalRecord(raws[id], &r); err == nil {
			fmt.Fprintf(h, "%s\x00%s\x00", id, canon)
		} else {
			fmt.Fprintf(h, "%s\x00!raw:%s\x00", id, string(raws[id]))
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// canonicalRecord decodes one snapshot entry as a memory record and renders
// it in stable form for fingerprinting.
func canonicalRecord(raw json.RawMessage, r *Record) ([]byte, error) {
	if err := json.Unmarshal(raw, r); err != nil || r.ID == "" {
		return nil, fmt.Errorf("not a memory record")
	}
	return json.Marshal(r)
}

// fingerprintSource digests the source snapshot; fingerprintIndex digests
// the index snapshot with the same function, so equal populations agree.
func fingerprintSource(snap map[string]sourceDoc) string {
	raws := make(map[string]json.RawMessage, len(snap))
	for id, d := range snap {
		raws[id] = d.raw
	}
	return fingerprintRecords(raws)
}

func fingerprintIndex(snap map[string]indexDoc) string {
	raws := make(map[string]json.RawMessage, len(snap))
	for id, d := range snap {
		raws[id] = d.raw
	}
	return fingerprintRecords(raws)
}

// shortFP renders the first 12 hex digits of a fingerprint for human lines.
func shortFP(fp string) string {
	hexpart := strings.TrimPrefix(fp, "sha256:")
	if len(hexpart) > 12 {
		hexpart = hexpart[:12]
	}
	return hexpart
}

// errDropped marks an index hit whose document is gone. runRetrieve counts
// it in freshness instead of serving it. errDroppedState marks a hit whose
// fresh record left the live lifecycle; it is counted separately.
var errDropped = fmt.Errorf("indexed hit names a record the collection no longer holds")

var errDroppedState = fmt.Errorf("indexed hit names a record outside the live lifecycle")

const (
	// SyncOp is the envelope op for an explicit index sync.
	SyncOp = "memory.index.sync"
	// MaxSyncRecords caps one sync: the source snapshot it will reconcile.
	// It equals the atomic batch bound, so a whole-index update stays one
	// batch; a larger collection fails before anything is written.
	MaxSyncRecords = dbfts.MaxLimit
	// MaxSyncBytes caps the snapshot bytes one sync reconciles.
	MaxSyncBytes = dbfts.MaxMaxBytes
)

// SyncQuery is one typed index-sync operation. It names the same two
// explicit targets as RetrieveQuery: the document source of truth and the
// full-text index to rebuild from it. Zero Limit/MaxBytes take the sync
// caps; a collection past them fails before any write.
type SyncQuery struct {
	Target   string // S11 db locator for the sqlite memory collection
	Index    string // S11 db locator for the bleve rank index
	Session  string // explicit owner session for a persistent target
	Dir      string // resolves a relative persistent path
	Limit    int    // max source records to reconcile (default MaxSyncRecords)
	MaxBytes int    // max snapshot bytes to reconcile (default MaxSyncBytes)
	Wait     time.Duration
}

// SyncResult is the result body of a successful sync envelope.
type SyncResult struct {
	SourceDocuments int    `json:"source_documents"`
	Added           int    `json:"added"`
	Updated         int    `json:"updated"`
	Deleted         int    `json:"deleted"`
	IndexDocuments  uint64 `json:"index_documents"`
}

// SyncIndex reconciles the full-text index with the memory collection: one
// bounded consistent source snapshot (a single doc.find read transaction),
// handed as input to the registered memory.index.sync category op, which
// reads the bounded index snapshot, diffs, and applies one atomic
// whole-index batch on the same admitted handle. The source snapshot is a
// separate explicit point — no cross-target atomicity is claimed — and
// freshness after the sync reports both revisions. A snapshot past the
// caps, or more operations than one batch holds, fails with limit_exceeded
// before anything is written. Like init, a sync is not one replayable owner
// request, so it takes no request ID.
func SyncIndex(ctx context.Context, q SyncQuery) Envelope {
	env := Envelope{SchemaVersion: RetrievalSchema, OK: true, Op: SyncOp,
		Collection: Collection, MutationClass: "write"}
	res, err := syncIndex(ctx, q, &env)
	if err != nil {
		return fail(env, err)
	}
	env.Result = res
	return env
}

func syncIndex(ctx context.Context, q SyncQuery, env *Envelope) (SyncResult, error) {
	var out SyncResult
	limit, maxBytes, err := syncBounds(q.Limit, q.MaxBytes)
	if err != nil {
		return out, err
	}
	docT, idxT, err := parseRetrieveTargets(RetrieveQuery{Target: q.Target, Index: q.Index, Session: q.Session, Dir: q.Dir})
	if err != nil {
		return out, err
	}
	docID, err := dbstore.Resolve(docT)
	if err != nil {
		return out, err
	}
	info := dblifecycle.ViewTarget(docID, "")
	env.Target = &info
	syncQ := RetrieveQuery{Target: q.Target, Index: q.Index, Session: q.Session, Dir: q.Dir, Wait: q.Wait}
	snap, err := snapshotSource(ctx, syncQ, docT, limit, maxBytes)
	if err != nil {
		return out, err
	}
	docs := make([]syncOpDoc, 0, len(snap))
	for id, doc := range snap {
		docs = append(docs, syncOpDoc{ID: id, Doc: doc.raw})
	}
	slices.SortFunc(docs, func(a, b syncOpDoc) int { return strings.Compare(a.ID, b.ID) })
	res, err := catOp(ctx, syncQ, idxT, SyncOp, syncOpInput{Collection: Collection, Documents: docs})
	if err != nil {
		return out, err
	}
	if err := decode(res.Result, &out); err != nil {
		return out, fmt.Errorf("%w: index sync result: %v", dbstore.ErrIntegrity, err)
	}
	return out, nil
}

func countNew(puts []dbfts.DocumentEntry, indexed map[string]indexDoc) int {
	n := 0
	for _, p := range puts {
		if _, ok := indexed[p.ID]; !ok {
			n++
		}
	}
	return n
}

func syncBounds(limit, maxBytes int) (int, int, error) {
	if limit == 0 {
		limit = MaxSyncRecords
	}
	if maxBytes == 0 {
		maxBytes = MaxSyncBytes
	}
	if limit < 1 || limit > MaxSyncRecords || maxBytes < 1 || maxBytes > MaxSyncBytes {
		return 0, 0, fmt.Errorf("%w: --limit 1..%d and --max-bytes 1..%d", dbstore.ErrInvalidInput, MaxSyncRecords, MaxSyncBytes)
	}
	return limit, maxBytes, nil
}

// sameRecord compares two stored source documents as records, so an export
// re-encoding of identical fields is not mistaken for an update.
func sameRecord(a indexDoc, raw json.RawMessage) bool {
	var x, y Record
	if err := json.Unmarshal([]byte(a.raw), &x); err != nil || x.ID == "" {
		return false
	}
	if err := json.Unmarshal(raw, &y); err != nil || y.ID == "" {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// sourceDoc is one memory record of a bounded source snapshot: the stored
// revision beside the stored bytes.
type sourceDoc struct {
	revision int64
	raw      json.RawMessage
}

// snapshotSource takes ONE bounded doc.find over the memory collection: one
// read transaction, one consistent point, no temp files, no unbounded
// export with the caps checked after allocation. Any next cursor or
// truncated page refuses with limit_exceeded before the caller writes
// anything to the index: doc.find pages are separate snapshots, so only a
// complete single page may feed a whole-index decision.
func snapshotSource(ctx context.Context, q RetrieveQuery, docT dbstore.Target, limit, maxBytes int) (map[string]sourceDoc, error) {
	out, err := catOp(ctx, q, docT, dbdoc.OpFind,
		dbdoc.Input{Collection: Collection, Limit: limit, MaxBytes: maxBytes})
	if err != nil {
		return nil, err
	}
	var res struct {
		Documents []dbdoc.Document `json:"documents"`
	}
	if err := decode(out.Result, &res); err != nil {
		return nil, fmt.Errorf("%w: source snapshot: %v", dbstore.ErrIntegrity, err)
	}
	var pg struct {
		Truncated bool            `json:"truncated"`
		Cursor    json.RawMessage `json:"cursor"`
	}
	if out.Page != nil {
		if err := decode(out.Page, &pg); err != nil {
			return nil, fmt.Errorf("%w: source snapshot page: %v", dbstore.ErrIntegrity, err)
		}
	}
	if pg.Truncated || (len(pg.Cursor) != 0 && string(pg.Cursor) != "null") {
		return nil, fmt.Errorf("%w: memory collection exceeds the snapshot cap (%d records, %d bytes); sync refuses a partial snapshot",
			dbstore.ErrLimitExceeded, limit, maxBytes)
	}
	snap := make(map[string]sourceDoc, len(res.Documents))
	for _, d := range res.Documents {
		var rec Record
		if err := json.Unmarshal(d.Doc, &rec); err != nil || rec.ID == "" || rec.ID != d.ID {
			return nil, fmt.Errorf("%w: snapshot record %q is not a memory record", dbstore.ErrIntegrity, d.ID)
		}
		if _, dup := snap[d.ID]; dup {
			return nil, fmt.Errorf("%w: snapshot holds %q twice", dbstore.ErrIntegrity, d.ID)
		}
		snap[d.ID] = sourceDoc{revision: d.Revision, raw: d.Doc}
	}
	return snap, nil
}

type indexDoc struct {
	revision string
	raw      json.RawMessage
}
