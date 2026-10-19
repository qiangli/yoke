// Semantic and hybrid memory retrieval, a K2 slice of story 56508548a7df.
//
// This file extends the shared Retrieve pipeline beside the lexical path in
// retrieval.go. It adds no second store and no second retrieval stack:
//
//   - the memory document target stays the source of truth for records,
//     revisions, deletions and lifecycle visibility (resolved through the
//     same source snapshot, the same kblife consumption check and the same
//     post-rank scope/time filter as lexical);
//   - the bleve full-text target stays the lexical rank source;
//   - an optional sqlite vector target holding one vec collection (default
//     "memory") is the semantic rank source. It is read only through the
//     existing vec category operations (vec.collection.list, vec.search)
//     through dblifecycle admission, exactly like every other db read.
//
// Embeddings go through the existing local model door only
// (pkg/broker/door): EmbedLocal posts one OpenAI-compatible /v1/embeddings
// request to the loopback door with the owner Bearer [REDACTED] and a bounded
// context, and refuses every redirect. There is no endpoint option, no
// provider registry and no cloud fallback: the door URL is derived from the
// door package, never from caller input. The lexical path (and the labeled
// lexical fallback) never embeds and never touches the network.
//
// An index-ingestion caller (the K2 producer) stores one vector per memory
// record under the stable record ID with the record JSON as the item
// metadata, in a collection whose persisted model and dimension name the
// embedding identity; EmbedLocal is the reusable helper for that side too.
// Retrieval validates the query against the collection through the vec
// category: the explicit --embed-model must match the collection's
// persisted model, and the embedded query dimension must match its
// persisted dimension.
//
// Modes: lexical is served exactly as before. Semantic ranks the bounded
// vector population by exact distance (nearest first, deterministic by
// record ID) and serves the live, filtered records. Hybrid fuses the
// bounded lexical and semantic lists with reciprocal rank fusion
// (score = sum of 1/(60+rank), deterministic by record ID) and serves the
// fused order. Filtering always runs before the limit. Explicit
// semantic/hybrid without a vector target and an embedding model (or an
// injected Embed func) remains structured unsupported; a caller that passes
// FallbackLexical gets labeled lexical results (RetrieveResult.Fallback)
// instead of the error.
//
// Freshness: the lexical side keeps its population fingerprints. The
// vector side cannot snapshot cheaply, so it reports the collection
// identity (target, model, dimension, item count) and Stale is true when
// the examined candidates show indexed bytes that differ from the served
// documents or any candidate is dropped.
package kbmemory

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/vecdb"
	"github.com/qiangli/yoke/pkg/dbvec"
)

const (
	// DefaultVectorCollection is the vec collection holding memory
	// embeddings when --vector-collection names none.
	DefaultVectorCollection = "memory"
	// rrfK is the reciprocal-rank-fusion constant shared by hybrid lists.
	rrfK = 60.0
	// embedTimeout bounds one local-door embedding call; the caller's
	// context still applies first when it ends sooner.
	embedTimeout = 60 * time.Second
	// maxEmbedResponse bounds one embedding response body.
	maxEmbedResponse = 4 << 20
	// maxEmbedTexts bounds one embedding call's input batch.
	maxEmbedTexts = 32
)

// Embedder turns rank texts into embeddings for one named model. A nil
// RetrieveQuery.Embed selects EmbedLocal; tests inject a fake.
type Embedder func(ctx context.Context, model string, texts []string) ([][]float32, error)

// EmbedLocal embeds texts with an explicit local model through the
// existing local model door only: one OpenAI-compatible POST to the
// loopback door's /v1/embeddings with the owner Bearer [REDACTED] It takes no
// endpoint, so there is no cloud fallback by construction. The context
// bounds the whole call (at most embedTimeout); redirects are never
// followed, so the token only ever goes to the door the caller named by
// running it locally.
func EmbedLocal(ctx context.Context, model string, texts []string) ([][]float32, error) {
	if strings.TrimSpace(model) == "" || strings.TrimSpace(model) != model {
		return nil, fmt.Errorf("%w: an explicit embedding model identity is required (no default model)", dbstore.ErrInvalidInput)
	}
	if len(texts) == 0 || len(texts) > maxEmbedTexts {
		return nil, fmt.Errorf("%w: embed takes 1..%d texts", dbstore.ErrInvalidInput, maxEmbedTexts)
	}
	for i, text := range texts {
		if text == "" || len(text) > dbvec.MaxTextBytes {
			return nil, fmt.Errorf("%w: text[%d] must be 1..%d bytes", dbstore.ErrLimitExceeded, i, dbvec.MaxTextBytes)
		}
	}
	token, err := door.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: local model door token: %v", dbstore.ErrBackend, err)
	}
	endpoint := door.BaseURL() + "/v1/embeddings"
	ctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	var input any = texts
	if len(texts) == 1 {
		input = texts[0]
	}
	body, err := json.Marshal(map[string]any{"model": model, "input": input, "encoding_format": "float"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: local model door: %v", dbstore.ErrInvalidInput, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := embedLocalClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, dbstore.FromContext(ctx.Err())
		}
		return nil, fmt.Errorf("%w: local model door %s: %v", dbstore.ErrBackend, endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: local model door %s answered %s (redirects are not followed)", dbstore.ErrBackend, endpoint, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbedResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, dbstore.FromContext(ctx.Err())
		}
		return nil, fmt.Errorf("%w: local model door %s: %v", dbstore.ErrBackend, endpoint, err)
	}
	if len(b) > maxEmbedResponse {
		return nil, fmt.Errorf("%w: embedding response is over %d bytes", dbstore.ErrLimitExceeded, maxEmbedResponse)
	}
	var out struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Data) != len(texts) {
		return nil, fmt.Errorf("%w: local model door %s did not return %d data[].embedding", dbstore.ErrBackend, endpoint, len(texts))
	}
	dim := -1
	vecs := make([][]float32, len(texts))
	for i, vec := range out.Data {
		if len(vec.Embedding) == 0 {
			return nil, fmt.Errorf("%w: local model door returned an empty embedding for text[%d]", dbstore.ErrBackend, i)
		}
		if dim < 0 {
			dim = len(vec.Embedding)
		} else if len(vec.Embedding) != dim {
			return nil, fmt.Errorf("%w: local model door returned mixed embedding dimensions", dbstore.ErrBackend)
		}
		vecs[i] = vec.Embedding
	}
	return vecs, nil
}

// embedLocalClient refuses every redirect, so the door Bearer [REDACTED] is
// only ever sent to the loopback door.
var embedLocalClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// resolveEmbedder is the embedding entry point for one retrieval: the
// injected func when the caller set one, the local door otherwise.
func (q RetrieveQuery) resolveEmbedder() Embedder {
	if q.Embed != nil {
		return q.Embed
	}
	return EmbedLocal
}

// vectorCollection names the vec collection holding memory embeddings.
func (q RetrieveQuery) vectorCollection() string {
	if strings.TrimSpace(q.VectorCollection) != "" {
		return strings.TrimSpace(q.VectorCollection)
	}
	return DefaultVectorCollection
}

// resolveMode returns the mode to serve. Explicit semantic/hybrid with a
// vector target and an embedding entry point (an explicit model or an
// injected Embed func) serves as named. Without them the read stays
// structured unsupported — unless the caller explicitly chose the labeled
// lexical fallback, which serves lexical with RetrieveResult.Fallback set.
func resolveMode(q RetrieveQuery) (mode string, fallback bool, err error) {
	switch strings.TrimSpace(q.Mode) {
	case "", ModeLexical:
		return ModeLexical, false, nil
	case ModeSemantic, ModeHybrid:
		name := strings.TrimSpace(q.Mode)
		if strings.TrimSpace(q.Vector) == "" || (strings.TrimSpace(q.EmbedModel) == "" && q.Embed == nil) {
			if q.FallbackLexical {
				return ModeLexical, true, nil
			}
			return "", false, fmt.Errorf("%w: mode %q needs an explicitly configured vector target (--vector) and embedding model (--embed-model or an injected Embed func); this read path offers lexical only unless --fallback-lexical labels a lexical fallback",
				dbstore.ErrUnsupported, name)
		}
		if strings.TrimSpace(q.Text) == "" {
			return "", false, fmt.Errorf("%w: mode %q needs rank --text (empty text is a lexical browse only)", dbstore.ErrInvalidInput, name)
		}
		return name, false, nil
	default:
		return "", false, fmt.Errorf("%w: mode %q (want lexical|semantic|hybrid)", dbstore.ErrInvalidInput, q.Mode)
	}
}

// parseVectorTarget resolves the optional semantic rank target: a sqlite
// locator holding the vec collection. Nothing here creates. A relative
// persistent path resolves under Dir, like the document and index targets.
func parseVectorTarget(q RetrieveQuery) (dbstore.Target, string, error) {
	t, err := dblifecycle.ParseTarget(q.Vector)
	if err != nil {
		return dbstore.Target{}, "", err
	}
	if t.Engine != Engine {
		return dbstore.Target{}, "", fmt.Errorf("%w: memory retrieval ranks vectors in the vector category (engine %s), not %s", dbstore.ErrUnsupported, Engine, t.Engine)
	}
	if t.Mode == dbstore.Memory && q.Session != "" && q.Session != t.Session {
		return dbstore.Target{}, "", fmt.Errorf("%w: --session %s does not own --vector session %s", dbstore.ErrInvalidInput, q.Session, t.Session)
	}
	if t.Mode == dbstore.Persistent && !filepath.IsAbs(t.Path) && q.Dir != "" {
		t.Path = filepath.Join(q.Dir, t.Path)
	}
	return t, q.vectorCollection(), nil
}

// vecCollectionDef is one vec collection as vec.collection.list reports it,
// with its live item count.
type vecCollectionDef struct {
	Name      string `json:"name"`
	Dimension int    `json:"dimension"`
	Metric    string `json:"metric"`
	Model     string `json:"model"`
	Vectors   int64  `json:"vectors"`
}

// loadVecCollection reads the vector collection definition through the vec
// category on the shared admission path: the persisted model and dimension
// are the embedding identity the query validates against. An absent
// collection is not_found naming it; nothing is created.
func loadVecCollection(ctx context.Context, q RetrieveQuery, vecT dbstore.Target, name string) (vecCollectionDef, error) {
	var def vecCollectionDef
	out, err := catOp(ctx, q, vecT, dbvec.OpCollectionList, dbvec.Input{})
	if err != nil {
		return def, err
	}
	var list struct {
		Collections []vecCollectionDef `json:"collections"`
	}
	if err := decode(out.Result, &list); err != nil {
		return def, fmt.Errorf("%w: vector collection list: %v", dbstore.ErrIntegrity, err)
	}
	for _, c := range list.Collections {
		if c.Name == name {
			if c.Dimension < 1 || c.Model == "" {
				return def, fmt.Errorf("%w: vector collection %q metadata is not an embedding collection", dbstore.ErrIntegrity, name)
			}
			return c, nil
		}
	}
	return def, fmt.Errorf("%w: vector collection %q (the K2 producer owns ingestion; retrieval never provisions it)", dbstore.ErrNotFound, name)
}

// vecSearchHit is one exact vector rank hit with its stored metadata: the
// K2 producer stores the memory record JSON there under the stable record
// ID, so staleness compares exactly like the lexical path.
type vecSearchHit struct {
	ID       string          `json:"id"`
	Revision int64           `json:"revision"`
	Distance float64         `json:"distance"`
	Metadata json.RawMessage `json:"metadata"`
}

// searchVectors ranks the bounded vector population by exact distance to
// the embedded query, nearest first and deterministic by ID, through the
// vec category on the shared admission path. The caller's model must match
// the collection's persisted model; the query dimension must match its
// persisted dimension.
func searchVectors(ctx context.Context, q RetrieveQuery, vecT dbstore.Target, def vecCollectionDef, query []float32) ([]vecSearchHit, error) {
	if len(query) != def.Dimension {
		return nil, fmt.Errorf("%w: embedding dimension %d; vector collection %q holds %d", dbstore.ErrInvalidInput, len(query), def.Name, def.Dimension)
	}
	tagged, err := json.Marshal(map[string]string{
		"t": "vec", "dtype": "f32", "b64": base64.StdEncoding.EncodeToString(vecdb.F32(query)),
	})
	if err != nil {
		return nil, err
	}
	// The vector category caps a read below the bleve population cap; the
	// list stays one bounded page either way.
	out, err := catOp(ctx, q, vecT, dbvec.OpSearch, dbvec.Input{
		Collection: def.Name, Vector: tagged, Model: def.Model,
		Limit: dbvec.MaxLimit, MaxBytes: dbvec.MaxMaxBytes,
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Hits []vecSearchHit `json:"hits"`
	}
	if err := decode(out.Result, &res); err != nil {
		return nil, fmt.Errorf("%w: vector search result: %v", dbstore.ErrIntegrity, err)
	}
	var page struct {
		Truncated bool            `json:"truncated"`
		Cursor    json.RawMessage `json:"cursor"`
	}
	if out.Page != nil {
		if err := decode(out.Page, &page); err != nil {
			return nil, err
		}
	}
	if page.Truncated || len(page.Cursor) > 0 && string(page.Cursor) != "null" {
		return nil, fmt.Errorf("%w: vector population exceeds the bounded retrieval snapshot", dbstore.ErrLimitExceeded)
	}
	hits := res.Hits
	slices.SortFunc(hits, func(a, b vecSearchHit) int {
		if a.Distance != b.Distance {
			if a.Distance < b.Distance {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	// A producer may store bounded chunks under recordID~N, each carrying
	// the unchanged source Record metadata. Collapse their best rank before
	// fusion and source/lifecycle filtering; return each source at most once.
	seen := map[string]bool{}
	sources := make([]vecSearchHit, 0, len(hits))
	for _, h := range hits {
		var record Record
		if json.Unmarshal(h.Metadata, &record) == nil && record.ID != "" && strings.HasPrefix(h.ID, record.ID+"~") {
			suffix := strings.TrimPrefix(h.ID, record.ID+"~")
			if _, err := strconv.ParseUint(suffix, 10, 32); err == nil {
				h.ID = record.ID
			}
		}
		if !seen[h.ID] {
			seen[h.ID] = true
			sources = append(sources, h)
		}
	}
	return sources, nil
}

// rankCandidate is one ordered rank entry awaiting source resolution: the
// stable record ID, the index revision label, the display score and the
// indexed bytes the staleness check compares against the served document.
type rankCandidate struct {
	id    string
	rev   string
	score float64
	doc   json.RawMessage
}

// fuseRRF orders the union of the bounded lexical and semantic lists by
// reciprocal rank fusion: each list contributes 1/(60+rank) per member
// (rank is 1-based in that list's served order), ties break by record ID.
// Either side may be empty; the union stays bounded by the two inputs.
func fuseRRF(lex []ftsHit, sem []vecSearchHit) []rankCandidate {
	scores := map[string]float64{}
	at := func(id string, rank int) {
		scores[id] += 1 / (rrfK + float64(rank))
	}
	docs := map[string]rankCandidate{}
	for i, h := range lex {
		at(h.ID, i+1)
		docs[h.ID] = rankCandidate{id: h.ID, rev: h.Revision, doc: h.Doc}
	}
	for i, h := range sem {
		at(h.ID, i+1)
		if _, ok := docs[h.ID]; !ok {
			docs[h.ID] = rankCandidate{id: h.ID, rev: "vec:" + strconv.FormatInt(h.Revision, 10), doc: h.Metadata}
		}
	}
	out := make([]rankCandidate, 0, len(scores))
	for id, score := range scores {
		c := docs[id]
		c.score = score
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b rankCandidate) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

// serveRanked resolves ordered rank candidates against the source snapshot
// the call already holds — the source of truth for liveness, filtering,
// revisions and staleness — and applies the byte budget and the requested
// limit after every filter, never before. It is the one serving loop for
// every mode: lexical builds its candidates from the bleve hits, semantic
// from the vector hits, hybrid from the fused union.
//
// popOnly keeps the lexical staleness contract byte for byte: Stale rests
// exactly on the population fingerprints the caller set, never on drops or
// per-hit staleness. Vector modes pass false, so examined staleness and
// drops also mark the population stale.
func serveRanked(ctx context.Context, snap map[string]sourceDoc, cands []rankCandidate, vecStale, popOnly bool,
	f retrieveFilter, why []string, mode string, limit, maxBytes int, asOf time.Time, out *RetrieveResult) error {
	used, byteStopped, staleHit := 0, false, false
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return dbstore.FromContext(err)
		}
		if len(out.Hits) >= limit {
			break
		}
		out.Freshness.Examined++
		hit, stop, err := serveSnapHit(snap, ftsHit{ID: c.id, Revision: c.rev, Score: c.score, Doc: c.doc}, f, why, used, maxBytes, asOf, mode)
		if err == errDropped {
			out.Freshness.DroppedStale++
			continue
		}
		if err == errDroppedState {
			out.Freshness.DroppedState++
			continue
		}
		if err != nil {
			return err
		}
		if hit != nil {
			if hit.hit.Stale {
				staleHit = true
			}
			out.Hits = append(out.Hits, hit.hit)
			used += hit.bytes
		}
		if stop {
			byteStopped = true
			break
		}
	}
	out.Freshness.Truncated = out.Freshness.Truncated || byteStopped ||
		(len(out.Hits) == limit && out.Freshness.Examined < len(cands))
	if !popOnly {
		out.Freshness.Stale = out.Freshness.Stale || vecStale || staleHit ||
			out.Freshness.DroppedStale > 0 || out.Freshness.DroppedState > 0
	}
	return nil
}

// runVectorRetrieve serves one semantic or hybrid retrieval. The document
// snapshot, lifecycle consumption, post-rank filter, revision reporting,
// per-hit stale labeling and byte budget are the same as lexical; only the
// rank order changes.
func runVectorRetrieve(ctx context.Context, q RetrieveQuery, mode string, docT, idxT dbstore.Target, docID dbstore.Identity, f retrieveFilter, limit, maxBytes int, asOf time.Time) (RetrieveResult, error) {
	out := RetrieveResult{Mode: mode, Hits: []Hit{}}
	out.Freshness.DocTarget = docID.ID
	vecT, collName, err := parseVectorTarget(q)
	if err != nil {
		return out, err
	}
	vecID, err := dbstore.Resolve(vecT)
	if err != nil {
		return out, err
	}
	def, err := loadVecCollection(ctx, q, vecT, collName)
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(q.EmbedModel) != "" && strings.TrimSpace(q.EmbedModel) != def.Model {
		return out, fmt.Errorf("%w: --embed-model %q; vector collection %q holds %q embeddings",
			dbstore.ErrInvalidInput, strings.TrimSpace(q.EmbedModel), def.Name, def.Model)
	}
	model := def.Model
	if strings.TrimSpace(q.EmbedModel) != "" {
		model = strings.TrimSpace(q.EmbedModel)
	}
	out.Freshness.VectorTarget = vecID.ID
	out.Freshness.VectorModel = def.Model
	out.Freshness.VectorDimension = def.Dimension
	out.Freshness.VectorDocuments = def.Vectors
	query, err := q.resolveEmbedder()(ctx, model, []string{q.Text})
	if err != nil {
		if q.FallbackLexical {
			res, ferr := runRetrieve(ctx, q, docT, idxT, docID, f, limit, maxBytes, asOf)
			if ferr != nil {
				return out, ferr
			}
			res.Mode, res.Fallback = ModeLexical, true
			return res, nil
		}
		return out, err
	}
	if len(query) != 1 {
		return out, fmt.Errorf("%w: embedding entry point returned %d vectors for one rank text", dbstore.ErrBackend, len(query))
	}
	sem, err := searchVectors(ctx, q, vecT, def, query[0])
	if err != nil {
		return out, err
	}
	srcSnap, err := snapshotSource(ctx, q, docT, MaxSyncRecords, MaxSyncBytes)
	if err != nil {
		return out, err
	}
	out.Freshness.SourceDocuments = int64(len(srcSnap))
	out.Freshness.SourceRevision = fingerprintSource(srcSnap)
	out.Freshness.IndexRevision = "vec:" + def.Model + ":" + strconv.Itoa(def.Dimension)
	why := f.why(q.Text)
	if mode == ModeSemantic {
		out.Freshness.IndexTarget = vecID.ID
		out.Freshness.IndexDocuments = uint64(def.Vectors)
		cands := make([]rankCandidate, 0, len(sem))
		for _, h := range sem {
			score := 1 / (1 + h.Distance)
			cands = append(cands, rankCandidate{id: h.ID,
				rev: "vec:" + strconv.FormatInt(h.Revision, 10), score: score, doc: h.Metadata})
		}
		if err := serveRanked(ctx, srcSnap, cands, false, false, f, why, mode, limit, maxBytes, asOf, &out); err != nil {
			return out, err
		}
		return out, nil
	}
	// Hybrid: the lexical side runs the same admitted index read as a
	// lexical recall, so its population fingerprint travels unchanged
	// beside the vector identity.
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
	if id, err := dbstore.Resolve(idxT); err == nil {
		out.Freshness.IndexTarget = id.ID
	}
	out.Freshness.IndexDocuments = rop.IndexDocuments
	out.Freshness.IndexRevision = rop.IndexRevision
	out.Freshness.Stale = out.Freshness.SourceRevision != out.Freshness.IndexRevision
	out.Freshness.IndexTotal = rop.Total
	out.Freshness.Truncated = rop.Truncated
	if err := serveRanked(ctx, srcSnap, fuseRRF(rop.Hits, sem), false, false, f, why, mode, limit, maxBytes, asOf, &out); err != nil {
		return out, err
	}
	return out, nil
}
