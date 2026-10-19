package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbvec"
	"github.com/qiangli/yoke/pkg/kbmemory"
)

// EnableEmbeddings is explicit setup: probe the named local model and create
// its named collection. Background work never provisions a model or a store.
func (m *Manager) EnableEmbeddings(ctx context.Context, model string) error {
	return m.locked(func() error {
		model = strings.TrimSpace(model)
		if model == "" {
			return fmt.Errorf("index: explicit model required")
		}
		if m.cfg.Model != "" && m.cfg.Model != model {
			return fmt.Errorf("index: model changes require a separate index home")
		}
		embed := m.Embed
		if embed == nil {
			embed = kbmemory.EmbedLocal
		}
		vectors, e := embed(ctx, model, []string{"embedding dimension"})
		if e != nil {
			return e
		}
		if len(vectors) != 1 || len(vectors[0]) == 0 {
			return fmt.Errorf("index: embedding model returned no vector")
		}
		_, e = cat(ctx, m.docTarget(), dbvec.OpCollectionAdd, dbvec.Input{Collection: "indexed", Model: model, Dimension: len(vectors[0]), Metric: "cosine"}, true)
		if e != nil && !errors.Is(e, dbstore.ErrExists) {
			return e
		}
		m.cfg.Model = model
		return m.save()
	})
}

const maxChunks = 66

func chunks(r kbmemory.Record) []string {
	text := r.Body
	out := []string{}
	for len(text) > 0 {
		n := min(4096, len(text))
		for n > 0 && !utf8.ValidString(text[:n]) {
			n--
		}
		if n == 0 {
			break
		}
		out = append(out, r.Title+"\n"+text[:n])
		text = text[n:]
	}
	if len(out) == 0 {
		out = append(out, r.Title)
	}
	return out
}
func (m *Manager) syncVectors(ctx context.Context) error {
	if m.cfg.Model == "" {
		return nil
	}
	docs, e := m.snapshot(ctx)
	if e != nil {
		return e
	}
	embed := m.Embed
	if embed == nil {
		embed = kbmemory.EmbedLocal
	}
	total := 0
	for _, d := range docs {
		var r kbmemory.Record
		if e = json.Unmarshal(d.Doc, &r); e != nil {
			return e
		}
		total += len(chunks(r))
	}
	if total > dbvec.MaxLimit {
		return fmt.Errorf("index: vector chunk count exceeds shared retrieval cap %d", dbvec.MaxLimit)
	}
	for id, d := range docs {
		var r kbmemory.Record
		if e = json.Unmarshal(d.Doc, &r); e != nil {
			return e
		}
		parts := chunks(r)
		first, err := cat(ctx, m.docTarget(), dbvec.OpGet, dbvec.Input{Collection: "indexed", ID: id + "~0"}, false)
		oldCount := 0
		if err == nil {
			b, _ := json.Marshal(first.Result)
			var item dbvec.Item
			if e = json.Unmarshal(b, &item); e != nil {
				return e
			}
			if bytes.Equal(item.Metadata, d.Doc) {
				if end := m.cfg.PendingTrim[id]; end > len(parts) {
					if err = m.trimVectors(ctx, id, len(parts), end); err != nil {
						return err
					}
					delete(m.cfg.PendingTrim, id)
					if err = m.save(); err != nil {
						return err
					}
				}
				continue
			}
			var prev kbmemory.Record
			if e = json.Unmarshal(item.Metadata, &prev); e != nil {
				return e
			}
			oldCount = len(chunks(prev))
		} else if !errors.Is(err, dbstore.ErrNotFound) {
			return err
		}
		if pending := m.cfg.PendingTrim[id]; pending > oldCount {
			oldCount = pending
		}
		if oldCount > len(parts) {
			if m.cfg.PendingTrim == nil {
				m.cfg.PendingTrim = map[string]int{}
			}
			m.cfg.PendingTrim[id] = oldCount
			if err = m.save(); err != nil {
				return err
			}
		}
		// The configured local model receives every bounded chunk, never an
		// undisclosed prefix of the source. Rank results collapse to source identity.
		for n, text := range parts {
			vid := id + "~" + strconv.Itoa(n)
			v, err := embed(ctx, m.cfg.Model, []string{text})
			if err != nil {
				return err
			}
			if len(v) != 1 {
				return fmt.Errorf("index: embedding count mismatch")
			}
			raw, _ := json.Marshal(v[0])
			in := dbvec.Input{Collection: "indexed", ID: vid, Vector: raw, Model: m.cfg.Model, Metadata: d.Doc}
			out, err := cat(ctx, m.docTarget(), dbvec.OpGet, dbvec.Input{Collection: "indexed", ID: vid}, false)
			if err == nil {
				b, _ := json.Marshal(out.Result)
				var item dbvec.Item
				if e = json.Unmarshal(b, &item); e != nil {
					return e
				}
				in.IfRevision = &item.Revision
			} else if errors.Is(err, dbstore.ErrNotFound) {
				in.IfAbsent = true
			} else {
				return err
			}
			if _, err = cat(ctx, m.docTarget(), dbvec.OpPut, in, false); err != nil {
				return err
			}
		}
		if err = m.trimVectors(ctx, id, len(parts), oldCount); err != nil {
			return err
		}
		if _, ok := m.cfg.PendingTrim[id]; ok {
			delete(m.cfg.PendingTrim, id)
			if err = m.save(); err != nil {
				return err
			}
		}

	}
	return nil
}
func (m *Manager) trimVectors(ctx context.Context, id string, start, end int) error {
	for n := start; n < end; n++ {
		_, e := cat(ctx, m.docTarget(), dbvec.OpRm, dbvec.Input{Collection: "indexed", ID: id + "~" + strconv.Itoa(n)}, false)
		if e != nil && !errors.Is(e, dbstore.ErrNotFound) {
			return e
		}
	}
	return nil
}
func (m *Manager) removeVector(ctx context.Context, id string) error {
	if m.cfg.Model == "" {
		return nil
	}
	for n := 0; n < maxChunks; n++ {
		_, e := cat(ctx, m.docTarget(), dbvec.OpRm, dbvec.Input{Collection: "indexed", ID: id + "~" + strconv.Itoa(n)}, false)
		if e != nil && !errors.Is(e, dbstore.ErrNotFound) {
			return e
		}
	}
	return nil
}
func (m *Manager) Query(ctx context.Context, text, mode string, limit int) kbmemory.Envelope {
	m = &Manager{dir: m.dir, Now: m.Now, Embed: m.Embed}
	if e := m.load(); e != nil {
		code := "integrity"
		if errors.Is(e, os.ErrNotExist) {
			code = "not_found"
		}
		return kbmemory.Envelope{SchemaVersion: kbmemory.ErrorSchema, OK: false, Error: &kbmemory.ErrorBody{Code: code, Message: e.Error()}}
	}

	doc, idx := m.Targets()
	auto := mode == "" || mode == "auto"
	if auto {
		mode = kbmemory.ModeHybrid
	}
	q := kbmemory.RetrieveQuery{Target: doc, Index: idx, Text: text, Mode: mode, Limit: limit, FallbackLexical: auto, Wait: wait}
	if m.cfg.Model != "" {
		q.Vector = doc
		q.VectorCollection = "indexed"
		q.EmbedModel = m.cfg.Model
		q.Embed = m.Embed
	}
	env := kbmemory.Retrieve(ctx, q)
	if env.OK && (m.cfg.Paused || !m.cfg.Enabled || m.cfg.LastError != "") {
		r := env.Result.(kbmemory.RetrieveResult)
		r.Freshness.Stale = true
		env.Result = r
	}
	return env
}
