package dbfts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/blevedb"
)

func init() {
	for _, name := range []string{OpSearch, OpGet, OpPut, OpDelete, OpBatch, OpCheck, OpMappingShow, OpExport, OpImport, OpRebuild} {
		name := name
		dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
			Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
				in, err := decodeInput(raw)
				if err != nil {
					return 0, err
				}
				if _, _, err := in.bounds(); err != nil {
					return 0, err
				}
				switch name {
				case OpPut, OpDelete, OpBatch:
					return dblifecycle.AccessWrite, nil
				case OpImport, OpRebuild, OpExport:
					return dblifecycle.AccessExclusive, nil
				default:
					return dblifecycle.AccessRead, nil
				}
			},
			Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
			Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
				in, err := decodeInput(raw)
				if err != nil {
					return dblifecycle.Outcome{}, err
				}
				if a != dblifecycle.AccessRead {
					leave, err := writerGate(h.Identity().ID).Enter(ctx, dbstore.DefaultWait)
					if err != nil {
						return dblifecycle.Outcome{}, err
					}
					defer leave()
				}
				return run(ctx, h, name, in)
			},
		})
	}
}

var gates sync.Map

func writerGate(id string) dbstore.Gate {
	v, _ := gates.LoadOrStore(id, dbstore.NewGate())
	return v.(dbstore.Gate)
}

var generationKey = []byte("bashy:generation")

func generation(idx bleve.Index) (uint64, error) {
	raw, err := idx.GetInternal(generationKey)
	if err != nil {
		return 0, err
	}
	if len(raw) == 0 {
		return 0, nil
	}
	n, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, dbstore.ErrIntegrity
	}
	return n, nil
}
func bumpGeneration(idx bleve.Index, batch *bleve.Batch) error {
	n, err := generation(idx)
	if err != nil {
		return err
	}
	batch.SetInternal(generationKey, []byte(strconv.FormatUint(n+1, 10)))
	return nil
}

func index(h dbstore.Handle) (*blevedb.DB, error) {
	d, ok := h.(*blevedb.DB)
	if !ok {
		return nil, fmt.Errorf("%w: fts requires bleve", dbstore.ErrUnsupported)
	}
	return d, nil
}

func sourceKey(id string) []byte { return []byte("bashy:source:" + id) }

func revision(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func document(raw json.RawMessage) (map[string]any, error) {
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return nil, fmt.Errorf("%w: document must be a JSON object", dbstore.ErrInvalidInput)
	}
	if _, reserved := doc["_bashy_source"]; reserved {
		return nil, fmt.Errorf("%w: reserved field _bashy_source", dbstore.ErrInvalidInput)
	}
	return doc, nil
}

func run(ctx context.Context, h dbstore.Handle, op string, in Input) (dblifecycle.Outcome, error) {
	d, err := index(h)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	idx := d.Index()
	switch op {
	case OpPut, OpDelete, OpBatch:
		entries := in.Documents
		deletes := in.Deletes
		if op == OpPut {
			entries = []DocumentEntry{{ID: in.ID, Doc: in.Doc, IfRevision: in.IfRevision}}
		}
		if op == OpDelete {
			deletes = []string{in.ID}
		}
		if len(entries)+len(deletes) == 0 {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: empty mutation", dbstore.ErrInvalidInput)
		}
		if len(entries)+len(deletes) > MaxLimit {
			return dblifecycle.Outcome{}, dbstore.ErrLimitExceeded
		}
		batch := idx.NewBatch()
		seen := make(map[string]bool)
		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return dblifecycle.Outcome{}, dbstore.FromContext(err)
			}
			if e.ID == "" || seen[e.ID] {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: empty or duplicate ID", dbstore.ErrInvalidInput)
			}
			seen[e.ID] = true
			doc, err := document(e.Doc)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			old, err := idx.GetInternal(sourceKey(e.ID))
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if old != nil && !in.Replace {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: document %s", dbstore.ErrExists, e.ID)
			}
			if old != nil && e.IfRevision == "" {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: if_revision required for update %s", dbstore.ErrConflict, e.ID)
			}
			if old == nil && e.IfRevision != "" || old != nil && e.IfRevision != "" && revision(old) != e.IfRevision {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: revision %s", dbstore.ErrConflict, e.ID)
			}
			if err := batch.Index(e.ID, doc); err != nil {
				return dblifecycle.Outcome{}, err
			}
			batch.SetInternal(sourceKey(e.ID), e.Doc)
		}
		for _, id := range deletes {
			if id == "" || seen[id] {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: empty or duplicate ID", dbstore.ErrInvalidInput)
			}
			seen[id] = true
			if old, err := idx.GetInternal(sourceKey(id)); err != nil {
				return dblifecycle.Outcome{}, err
			} else if old == nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: document %s", dbstore.ErrNotFound, id)
			} else if op == OpDelete && in.IfRevision != "" && revision(old) != in.IfRevision {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: revision %s", dbstore.ErrConflict, id)
			}
			batch.Delete(id)
			batch.DeleteInternal(sourceKey(id))
		}
		if err := ctx.Err(); err != nil {
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		if err := bumpGeneration(idx, batch); err != nil {
			return dblifecycle.Outcome{}, err
		}
		if err := idx.Batch(batch); err != nil {
			return dblifecycle.Outcome{}, err
		}
		return dblifecycle.Outcome{Result: map[string]any{"updated": len(entries), "deleted": len(deletes)}}, nil
	case OpGet:
		if in.ID == "" {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: ID required", dbstore.ErrInvalidInput)
		}
		raw, err := idx.GetInternal(sourceKey(in.ID))
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if raw == nil {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: document %s", dbstore.ErrNotFound, in.ID)
		}
		return dblifecycle.Outcome{Result: map[string]any{"id": in.ID, "revision": revision(raw), "doc": json.RawMessage(raw)}}, nil
	case OpSearch:
		limit, maxBytes, err := in.bounds()
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		q := bleve.NewMatchAllQuery()
		var query query.Query = q
		if in.Query != "" {
			m := bleve.NewMatchQuery(in.Query)
			m.SetField(in.Field)
			query = m
		}
		if in.Filter != "" {
			query = bleve.NewConjunctionQuery(query, bleve.NewQueryStringQuery(in.Filter))
		}
		gen, err := generation(idx)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		binding := fmt.Sprintf("%s:%s:%s:%s:%d", d.Identity().ID, in.Query, in.Field, in.Filter, gen)
		sum := sha256.Sum256([]byte(binding))
		key := hex.EncodeToString(sum[:8])
		offset := 0
		if in.Since != "" {
			parts := strings.Split(in.Since, ":")
			if len(parts) != 3 || parts[0] != "bashy-cursor-v1" || parts[1] != key {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: cursor target/query mismatch", dbstore.ErrInvalidInput)
			}
			offset, err = strconv.Atoi(parts[2])
			if err != nil || offset < 0 {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: cursor offset", dbstore.ErrInvalidInput)
			}
		}
		req := bleve.NewSearchRequestOptions(query, limit, offset, false)
		req.SortBy([]string{"_id"})
		res, err := idx.SearchInContext(ctx, req)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		hits := make([]map[string]any, 0, len(res.Hits))
		bytesUsed := 0
		for _, hit := range res.Hits {
			raw, err := idx.GetInternal(sourceKey(hit.ID))
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if raw == nil {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: source missing for %s", dbstore.ErrIntegrity, hit.ID)
			}
			if bytesUsed+len(raw) > maxBytes {
				if len(hits) == 0 {
					return dblifecycle.Outcome{}, fmt.Errorf("%w: first document exceeds max_bytes", dbstore.ErrLimitExceeded)
				}
				break
			}
			hits = append(hits, map[string]any{"id": hit.ID, "revision": revision(raw), "score": hit.Score, "doc": json.RawMessage(raw)})
			bytesUsed += len(raw)
		}
		endGen, err := generation(idx)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if endGen != gen {
			return dblifecycle.Outcome{}, fmt.Errorf("%w: index changed during search", dbstore.ErrConflict)
		}
		next := ""
		if uint64(offset+len(hits)) < res.Total {
			next = fmt.Sprintf("bashy-cursor-v1:%s:%d", key, offset+len(hits))
		}
		return dblifecycle.Outcome{Result: map[string]any{"total": res.Total, "hits": hits}, Page: map[string]any{"next": next, "returned": len(hits), "limit": limit, "max_bytes": maxBytes}}, nil
	case OpMappingShow:
		return dblifecycle.Outcome{Result: idx.Mapping()}, nil
	case OpCheck:
		count, err := idx.DocCount()
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		sources, err := allSources(ctx, d)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if uint64(len(sources)) != count {
			return dblifecycle.Outcome{}, dbstore.ErrIntegrity
		}
		return dblifecycle.Outcome{Result: map[string]any{"documents": count, "valid": true}}, nil
	case OpExport:
		return runExport(ctx, d, in)
	case OpImport:
		return runImport(ctx, d, in)
	case OpRebuild:
		return runRebuild(ctx, d, in)
	}
	return dblifecycle.Outcome{}, dbstore.ErrUnsupported
}
