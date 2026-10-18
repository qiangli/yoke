// Bounded provenance-rich context over one explicit memory collection.
//
// MemoryContext assembles agent context through the EXISTING kb context
// command and the frozen context_version 1 envelope. The lexical pipeline
// is exactly one call: kbmemory.Retrieve, the shared scoped retrieval the
// Wave0 acceptance (story a566e6387674) owns. Nothing here re-implements
// ranking, budgeting or dedup: served records re-enter the existing
// assembler (Context) as one ring's hits, so per-ring ranking, the per-ring
// K cap, the cue→line→full budget ladder and the multi-hop walk behave
// exactly as they do for kb rings.
//
// What this file adds, and only this:
//
//   - MemorySource: the explicit collection binding (document target plus
//     full-text index, both S11 db locators). No default collection, no
//     implicit creation: a missing target fails with not_found naming it,
//     exactly as Retrieve reports it.
//   - The hit mapping: stable record ID verbatim as the block ref and the
//     source[].uri (§10.3), lifecycle and status passthrough, retrieval
//     reasons as why[], and one additive per-block retrieval object
//     (mode, score, canonical target, estimate marker, revisions).
//   - Two additive top-level objects: freshness (which index population
//     was ranked) and truncation (retrieval cuts plus assembler omissions).
//   - Internal structured failure state (never marshalled: §10.3 admits
//     no further top-level field) when the explicit retrieval fails, so
//     the CLI/MCP boundary can exit with the retrieval code.
//
// Additive contract (§10.3): every new envelope field is omitempty and nil
// for legacy kb-ring blocks, so the golden testdata/kb-context-envelope.json
// stays byte-unchanged and no field is repurposed. Data stays data:
// retrieved text is rendered as cited bullets for the caller to paste
// deliberately; nothing is injected into prompts.
//
// Token accounting: memory blocks cost through the same estimator as every
// other block (rendered words plus the per-block envelope allowance), so
// Budget.Used already covers the new collection path with no envelope
// change. The contract's tokenizer-or-estimate rule is met inside the new
// per-block retrieval object: estimate:true labels the block's Tokens as
// an estimate, and the prompt text marks new-path blocks the same way.
package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

// MemorySource names one explicit memory collection for context assembly:
// the document target (source of truth) plus the full-text rank index.
// Both are S11 db locators; either mode works — a persistent file target,
// or an in-memory target through its explicit owner session.
type MemorySource struct {
	// Target is the sqlite memory collection locator (required).
	Target string
	// Index is the bleve rank index locator (required; never provisioned).
	Index string
	// Session is the explicit owner session for a persistent target, or
	// empty for the direct path. A memory-mode target resolves its own
	// session from the locator.
	Session string
	// Dir resolves a relative persistent path.
	Dir string
	// Mode is "" (lexical) or "lexical". Semantic and hybrid are refused
	// explicitly by Retrieve: there is no embedding backend on this path
	// and no silent substitution.
	Mode string
	// Ring is the emission ring for served records. Empty means the agent
	// ring: memory collections are agent-owned working memory, and the
	// per-block retrieval object plus the source target tell them apart
	// from kb agent notes. No new ring name is introduced.
	Ring string
	// Tags, Repos, OS, Since and Until are the conjunctive post-rank
	// scope, passed straight to Retrieve. Empty means unfiltered. The
	// context query's ambient repo/OS do NOT filter memory records: a
	// record captured without scope must still be served.
	Tags         []string
	Repos        []string
	OS           string
	Since, Until string
	// Wait bounds lock/admission waits; Timeout bounds the whole read.
	// Zero Wait takes the lifecycle default; zero Timeout means no bound.
	Wait, Timeout time.Duration
	// Call runs retrieval; nil means kbmemory.Retrieve. Tests stub it.
	Call func(ctx context.Context, q kbmemory.RetrieveQuery) kbmemory.Envelope
}

// ring resolves the emission ring.
func (s MemorySource) ring() string {
	if strings.TrimSpace(s.Ring) != "" {
		return strings.TrimSpace(s.Ring)
	}
	return RingAgent
}

// mode resolves the retrieval mode, defaulting to lexical.
func (s MemorySource) mode() string {
	if strings.TrimSpace(s.Mode) != "" {
		return strings.TrimSpace(s.Mode)
	}
	return kbmemory.ModeLexical
}

// Reader returns the Reader facet of this source for selection validation:
// the ring it emits under and the forms memory records may carry. It holds
// no hits; MemoryContext fills a twin with the served records.
func (s MemorySource) Reader() Reader {
	return memoryHitsReader{ring: s.ring(), forms: memoryForms()}
}

// memoryForms is every form a memory record may carry. Capture stores
// notes today; the adapter (like M4 seeds) passes any of these through
// rather than inventing a default the record never had.
func memoryForms() []string {
	return []string{kb.FormNote, kb.FormPage, kb.FormRelation, kb.FormCode}
}

// BlockRetrieval is the per-block lexical provenance for a block assembled
// from a memory collection. Nil for legacy kb-ring blocks; omitempty keeps
// the frozen envelope byte-unchanged.
type BlockRetrieval struct {
	Mode  string  `json:"mode"`
	Score float64 `json:"score"`
	// Target is the canonical document target the record was served from.
	// The stable record ID stays the block ref and SourceRef URI verbatim
	// (§10.3); the target travels here and in top-level freshness, never
	// folded into the ref.
	Target string `json:"target,omitempty"`
	// Estimate marks the block's Tokens as an estimator label, not a
	// tokenizer measurement: the assembler word-count covers the new
	// collection path exactly as it covers kb rings. Always true on
	// memory blocks; absent on legacy blocks (nil Retrieval).
	Estimate bool `json:"estimate,omitempty"`
	// Revision is the source-of-truth revision; IndexRevision is the
	// revision the rank index holds. Stale marks a hit whose indexed bytes
	// differed from the served document.
	Revision      int64  `json:"revision,omitempty"`
	IndexRevision string `json:"index_revision,omitempty"`
	Stale         bool   `json:"stale,omitempty"`
}

// MemoryTruncation reports a cut read. Present when the retrieval was
// truncated OR when the assembler left served memory hits out: a silent
// cut is never acceptable, an absent cut needs no object. Examined and
// the Dropped counts mirror the retrieval freshness report; Omitted
// counts served memory hits with no surviving block — the per-ring K cap,
// the budget ladder, or cross-source dedup — so assembler cuts are
// reported alongside retrieval cuts.
type MemoryTruncation struct {
	Truncated    bool `json:"truncated"`
	Examined     int  `json:"examined,omitempty"`
	DroppedStale int  `json:"dropped_stale,omitempty"`
	DroppedState int  `json:"dropped_state,omitempty"`
	Omitted      int  `json:"omitted,omitempty"`
}

// MemoryFreshness names the population the context was ranked from: the
// canonical targets plus the source/index fingerprints and whether they
// disagreed. A read during a rebuild either used the last complete index
// (stale) or failed busy (a broken-ring error, never a mixed generation).
type MemoryFreshness struct {
	DocTarget      string `json:"doc_target"`
	IndexTarget    string `json:"index_target,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	IndexRevision  string `json:"index_revision,omitempty"`
	Stale          bool   `json:"stale,omitempty"`
}

// MemoryContext assembles context over the legacy readers plus one explicit
// memory collection. Retrieve runs once under ctx; on error the packet
// FAILS — the collection is explicit, so a retrieval failure is a caller
// error, not a partial answer. The legacy rings still run so their
// statuses stay honest, but their blocks are withheld, the memory ring is
// recorded broken, and the structured retrieval code rides the internal
// MemoryError state so the CLI/MCP boundary exits with it (never a
// successful legacy-only packet, never process exit inside the library).
// On success the served records are deduplicated by (target, record ID)
// BEFORE entering Context, so duplicates never starve unique hits out of
// the budget; there is no post-assembly identity collapse.
func MemoryContext(ctx context.Context, q Query, src MemorySource, readers ...Reader) ContextResult {
	asOf := ""
	if !q.AsOf.IsZero() {
		asOf = q.asOf().UTC().Format(time.RFC3339Nano)
	}
	call := src.Call
	if call == nil {
		call = kbmemory.Retrieve
	}
	if src.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, src.Timeout)
		defer cancel()
	}
	env := call(ctx, kbmemory.RetrieveQuery{
		Mode: src.mode(), Target: src.Target, Index: src.Index,
		Session: src.Session, Dir: src.Dir,
		Text: q.Text, Tags: src.Tags, Repos: src.Repos, OS: src.OS,
		Since: src.Since, Until: src.Until,
		Limit: q.k(), AsOf: asOf, Wait: src.Wait,
	})
	if env.Error != nil || !env.OK {
		code := "backend_error"
		msg := "memory.retrieve failed"
		if env.Error != nil {
			if strings.TrimSpace(env.Error.Code) != "" {
				code = strings.TrimSpace(env.Error.Code)
			}
			if strings.TrimSpace(env.Error.Message) != "" {
				msg = env.Error.Message
			}
		}
		return contextWithMemoryError(q, readers, src.ring(), code, msg)
	}
	var res kbmemory.RetrieveResult
	switch r := env.Result.(type) {
	case kbmemory.RetrieveResult:
		res = r
	case *kbmemory.RetrieveResult:
		if r != nil {
			res = *r
		}
	default:
		raw, err := json.Marshal(env.Result)
		if err != nil {
			return contextWithRingError(q, readers, src.ring(), fmt.Sprintf("decode result: %v", err))
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return contextWithRingError(q, readers, src.ring(), fmt.Sprintf("decode result: %v", err))
		}
	}
	if !q.allowsRing(src.ring()) {
		return contextWithRingError(q, readers, src.ring(),
			fmt.Sprintf("memory ring %q is not selected (rings: %s)", src.ring(), strings.Join(q.Rings, ",")))
	}
	canonical := strings.TrimSpace(res.Freshness.DocTarget)
	if canonical == "" {
		canonical = strings.TrimSpace(src.Target)
	}
	hits := make([]Hit, 0, len(res.Hits))
	for _, h := range res.Hits {
		if len(q.Types) > 0 && !slices.Contains(q.Types, h.Record.Type) {
			continue
		}
		hits = append(hits, memoryHit(src, canonical, h))
	}
	// Dedup BEFORE the assembler spends budget: two served rows naming
	// the same (target, record ID) are one record, and the duplicate must
	// not starve a unique hit out of the per-ring K cap or the budget
	// ladder. The verbatim ref is preserved on the survivor.
	hits = dedupeMemoryHits(canonical, hits)
	cp := make([]Reader, 0, len(readers)+1)
	cp = append(cp, readers...)
	cp = append(cp, memoryHitsReader{ring: src.ring(), forms: memoryForms(), hits: hits})
	out := Context(q, cp...)
	// No post-assembly collapse: a memory ref string under a kb ring
	// names an unrelated record from a distinct target, and (ring, ref)
	// dedup would silently drop it. Pre-budget target+ID dedup above is
	// the only identity collapse on this path.
	surviving := 0
	for _, b := range out.Blocks {
		if b.Retrieval != nil {
			surviving++
		}
	}
	omitted := len(hits) - surviving
	if omitted < 0 {
		omitted = 0
	}
	out.Freshness = &MemoryFreshness{
		DocTarget:      res.Freshness.DocTarget,
		IndexTarget:    res.Freshness.IndexTarget,
		SourceRevision: res.Freshness.SourceRevision,
		IndexRevision:  res.Freshness.IndexRevision,
		Stale:          res.Freshness.Stale,
	}
	if res.Freshness.Truncated || omitted > 0 {
		out.Truncation = &MemoryTruncation{
			Truncated:    true,
			Examined:     res.Freshness.Examined,
			DroppedStale: res.Freshness.DroppedStale,
			DroppedState: res.Freshness.DroppedState,
			Omitted:      omitted,
		}
	}
	return out
}

// contextWithMemoryError fails the packet: the collection was explicit,
// so a retrieval failure is a caller error and the legacy blocks are
// withheld (never a successful legacy-only packet). The legacy readers
// still run so their ring statuses stay honest; the memory ring is
// recorded broken with "code: message", and MemoryError carries the
// structured code for the CLI/MCP exit mapping.
func contextWithMemoryError(q Query, readers []Reader, ring, code, msg string) ContextResult {
	res := Context(q, readers...)
	res.Blocks = []ContextBlock{}
	res.Budget.Used = 0
	res.Abstained = false
	res.MemoryError = &MemoryRetrieveError{Code: code, Message: msg}
	display := msg
	if code != "" {
		display = code + ": " + msg
	}
	for i := range res.Rings {
		if res.Rings[i].Name == ring {
			res.Rings[i].OK = false
			if res.Rings[i].Error == "" {
				res.Rings[i].Error = display
			} else {
				res.Rings[i].Error = display + "; " + res.Rings[i].Error
			}
			return res
		}
	}
	res.Rings = append(res.Rings, ContextRing{Name: ring, Error: display})
	return res
}

// contextWithRingError answers the legacy rings and records the named
// ring as broken without withholding blocks. It serves selection refusals
// (the memory ring is simply not selected), where the retrieval itself
// did not fail — retrieval failures take contextWithMemoryError instead.
func contextWithRingError(q Query, readers []Reader, ring, msg string) ContextResult {
	res := Context(q, readers...)
	for i := range res.Rings {
		if res.Rings[i].Name == ring {
			if res.Rings[i].OK {
				res.Rings[i].OK = false
				res.Rings[i].Error = msg
			} else if res.Rings[i].Error == "" {
				res.Rings[i].Error = msg
			}
			return res
		}
	}
	return ContextResult{
		ContextVersion: res.ContextVersion,
		For:            res.For,
		Memory:         res.Memory,
		Budget:         res.Budget,
		Abstained:      res.Abstained,
		Rings:          append(res.Rings, ContextRing{Name: ring, Error: msg}),
		Blocks:         res.Blocks,
	}
}

// MemoryExitCode maps a memory retrieval error code to the process exit
// the CLI uses. It mirrors the kbmemory envelope table (db contract
// §5.2): timeout and cancellation keep their 124/130 so a caller can
// tell them from an ordinary failure.
func MemoryExitCode(code string) int {
	switch strings.TrimSpace(code) {
	case "invalid_input", "unsupported", "exclusion_unsupported":
		return 2
	case "claim_held", "stale_epoch":
		return 9
	case "timeout":
		return 124
	case "cancelled":
		return 130
	default:
		return 1
	}
}

// memoryHit maps one served record to a recall hit. The block ref and the
// source URI are the stable record ID verbatim (§10.3); the canonical
// document target travels in the Retrieval object and top-level freshness,
// never folded into the ref. Why carries the retrieval reasons, and the
// Retrieval provenance object rides the Hit seam (recall.go) into the
// budgeted block.
func memoryHit(src MemorySource, canonicalTarget string, h kbmemory.Hit) Hit {
	form := strings.TrimSpace(h.Record.Form)
	switch form {
	case kb.FormNote, kb.FormPage, kb.FormRelation, kb.FormCode:
	default:
		form = kb.FormPage
	}
	body := strings.TrimSpace(h.Record.Body)
	gist := body
	if len([]rune(gist)) > 120 {
		gist = string([]rune(gist)[:120])
	}
	// Only lexical is ever served; a hit that names no mode still gets
	// the source's resolved mode so the provenance is never blank.
	mode := strings.TrimSpace(h.Mode)
	if mode == "" {
		mode = src.mode()
	}
	target := strings.TrimSpace(canonicalTarget)
	if target == "" {
		target = strings.TrimSpace(src.Target)
	}
	return Hit{
		Ring: src.ring(), Kind: "memory", ID: h.Ref, Form: form,
		Cue: strings.TrimSpace(h.Record.Title), Gist: gist, Full: body,
		Source:     []SourceRef{{URI: h.Ref}},
		Status:     strings.TrimSpace(h.Record.Status),
		Confidence: "RETRIEVED",
		ObservedAt: strings.TrimSpace(h.Record.Updated),
		ValidFrom:  strings.TrimSpace(h.Record.ValidFrom),
		ValidTo:    strings.TrimSpace(h.Record.ValidTo),
		Score:      h.Score,
		Why:        append(append([]string{"memory"}, h.Why...), mode),
		Retrieval: &BlockRetrieval{
			Mode:          mode,
			Score:         h.Score,
			Target:        target,
			Estimate:      true,
			Revision:      h.Revision,
			IndexRevision: strings.TrimSpace(h.IndexRevision),
			Stale:         h.Stale,
		},
	}
}

// dedupeMemoryHits drops later hits repeating an earlier (target, record
// ID). It runs BEFORE the assembler spends budget so a duplicate never
// starves a unique hit. The identity includes the target: two collections
// may mint the same record ID for unrelated records, and those must never
// collapse. Empty refs never dedupe.
func dedupeMemoryHits(target string, hits []Hit) []Hit {
	seen := map[string]bool{}
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		if h.ID != "" {
			key := target + "\x00" + h.ID
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, h)
	}
	return out
}

// memoryHitsReader is one ring's served memory records. It is built fresh
// per MemoryContext call and carries no state beyond the hits.
type memoryHitsReader struct {
	ring  string
	forms []string
	hits  []Hit
}

func (r memoryHitsReader) Ring() string    { return r.ring }
func (r memoryHitsReader) Forms() []string { return append([]string(nil), r.forms...) }
func (r memoryHitsReader) Recall(Query) ([]Hit, error) {
	return append([]Hit(nil), r.hits...), nil
}
