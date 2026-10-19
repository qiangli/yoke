// Package indexer composes managed source ingestion with the shared document,
// full-text and retrieval tools. Indexed sources never become curated kb pages.
package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/dbdoc"
	_ "github.com/qiangli/yoke/pkg/dbfts"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/kbmemory"
	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/secrets"
)

const wait = 5 * time.Second

func (m *Manager) docTarget() dbstore.Target {
	return dbstore.Target{Engine: "sqlite", Mode: dbstore.Persistent, Path: filepath.Join(m.dir, "documents.db")}
}
func (m *Manager) indexTarget() dbstore.Target {
	return dbstore.Target{Engine: "bleve", Mode: dbstore.Persistent, Path: filepath.Join(m.dir, "fulltext")}
}
func locator(t dbstore.Target) string {
	return (ref.Locator{Scheme: "db", Target: t.Engine + ":file:" + t.Path}).String()
}
func (m *Manager) Targets() (string, string) { return locator(m.docTarget()), locator(m.indexTarget()) }
func cat(ctx context.Context, t dbstore.Target, op string, in any, exclusive bool) (dblifecycle.Outcome, error) {
	b, e := json.Marshal(in)
	if e != nil {
		return dblifecycle.Outcome{}, e
	}
	_, out, e := (dblifecycle.Service{}).RunCategory(ctx, t, op, b, exclusive, wait)
	return out, e
}
func (m *Manager) initStores(ctx context.Context) error {
	for _, t := range []dbstore.Target{m.docTarget(), m.indexTarget()} {
		_, err := (dblifecycle.Service{}).Create(ctx, t, wait)
		if err != nil && !errors.Is(err, dbstore.ErrExists) {
			return err
		}
	}
	_, err := cat(ctx, m.docTarget(), dbdoc.OpCollectionCreate, dbdoc.Input{Collection: kbmemory.Collection}, true)
	if errors.Is(err, dbstore.ErrExists) {
		err = nil
	}
	return err
}
func (m *Manager) snapshot(ctx context.Context) (map[string]dbdoc.Document, error) {
	out, e := cat(ctx, m.docTarget(), dbdoc.OpFind, dbdoc.Input{Collection: kbmemory.Collection, Limit: kbmemory.MaxSyncRecords, MaxBytes: kbmemory.MaxSyncBytes}, false)
	if e != nil {
		return nil, e
	}
	var page struct {
		Truncated bool            `json:"truncated"`
		Cursor    json.RawMessage `json:"cursor"`
	}
	if out.Page != nil {
		b, _ := json.Marshal(out.Page)
		if e = json.Unmarshal(b, &page); e != nil {
			return nil, e
		}
	}
	if page.Truncated || len(page.Cursor) > 0 && string(page.Cursor) != "null" {
		return nil, fmt.Errorf("index: source snapshot exceeds shared retrieval limits")
	}
	var res struct {
		Documents []dbdoc.Document `json:"documents"`
	}
	b, _ := json.Marshal(out.Result)
	if e = json.Unmarshal(b, &res); e != nil {
		return nil, e
	}
	docs := make(map[string]dbdoc.Document, len(res.Documents))
	for _, d := range res.Documents {
		docs[d.ID] = d
	}
	return docs, nil
}
func sourceID(unit, rel string) string {
	sum := sha256.Sum256([]byte(unit + "\x00" + rel))
	return hex.EncodeToString(sum[:])
}
func scrubText(text string) string {
	s := secrets.NewRedactor()
	_ = s.SetShapeMode(secrets.ShapeMask)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		if ok && (strings.Contains(u, "TOKEN") || strings.Contains(u, "PASSWORD") || strings.Contains(u, "SECRET") || strings.Contains(u, "API_KEY")) {
			_ = s.Register(k, v)
		}
	}
	return redact.FromHost().String(string(s.Redact([]byte(text))))
}
func (m *Manager) put(ctx context.Context, id, path, text string, old *dbdoc.Document) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := kbmemory.Record{ID: id, Form: "note", Type: "fact", Title: filepath.Base(path), Body: scrubText(text), Status: "candidate", Tags: []string{"indexed-file"}, Source: kbmemory.Source{Tool: "index", Ref: sourceURI(path)}, Created: now, Updated: now, ValidFrom: now}
	in := dbdoc.Input{Collection: kbmemory.Collection, ID: id, IfAbsent: old == nil}
	if old != nil {
		var prev kbmemory.Record
		if e := json.Unmarshal(old.Doc, &prev); e != nil {
			return e
		}
		if prev.Body == rec.Body && prev.Source.Ref == rec.Source.Ref {
			return nil
		}
		rec.Created, rec.ValidFrom = prev.Created, prev.ValidFrom
		in.IfRevision = &old.Revision
	}
	in.Doc, _ = json.Marshal(rec)
	_, err := cat(ctx, m.docTarget(), dbdoc.OpPut, in, false)
	return err
}
func (m *Manager) remove(ctx context.Context, d dbdoc.Document) error {
	if e := m.removeVector(ctx, d.ID); e != nil {
		return e
	}
	_, e := cat(ctx, m.docTarget(), dbdoc.OpRm, dbdoc.Input{Collection: kbmemory.Collection, ID: d.ID, IfRevision: &d.Revision}, false)
	return e
}
func (m *Manager) syncIndex(ctx context.Context) error {
	doc, idx := m.Targets()
	e := kbmemory.SyncIndex(ctx, kbmemory.SyncQuery{Target: doc, Index: idx, Wait: wait})
	if !e.OK {
		return fmt.Errorf("index sync: %s", e.Error.Message)
	}
	return nil
}
