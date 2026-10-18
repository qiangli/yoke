package dbfts

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/blevedb"
)

func TestBleveInternalStorage(t *testing.T) {
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	tg := dbstore.Target{Mode: dbstore.Memory, Session: s.ID(), Name: "test"}
	h, err := blevedb.Adapter{}.Create(ctx, tg, dbstore.Options{Mem: s})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	d := h.(*blevedb.DB)
	idx := d.Index()

	// Test SetInternal & GetInternal
	if err := idx.SetInternal([]byte("doc:1"), []byte(`{"title":"hello"}`)); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}
	val, err := idx.GetInternal([]byte("doc:1"))
	if err != nil {
		t.Fatalf("GetInternal: %v", err)
	}
	if string(val) != `{"title":"hello"}` {
		t.Fatalf("unexpected val: %s", string(val))
	}

	// Index doc
	if err := idx.Index("1", map[string]any{"title": "hello"}); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Search
	res, err := idx.Search(bleve.NewSearchRequest(bleve.NewMatchQuery("hello")))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("expected 1 hit, got %d", res.Total)
	}

	// Check fields
	fields, err := idx.Fields()
	if err != nil {
		t.Fatalf("Fields: %v", err)
	}
	t.Logf("Fields: %v", fields)

	// Check FieldDict for title
	dict, err := idx.FieldDict("title")
	if err != nil {
		t.Fatalf("FieldDict: %v", err)
	}
	entry, err := dict.Next()
	if err != nil {
		t.Fatalf("dict.Next: %v", err)
	}
	if entry != nil {
		t.Logf("title term: %s, count: %d", entry.Term, entry.Count)
	}

	// Check FieldDict for _id
	dictID, err := idx.FieldDict("_id")
	if err != nil {
		t.Logf("FieldDict(_id) err: %v", err)
	} else {
		for {
			e, err := dictID.Next()
			if err != nil || e == nil {
				break
			}
			t.Logf("_id term: %s", e.Term)
		}
	}
}

func TestBlevePersistentInternalStorage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bleve_idx")
	tg := dbstore.Target{Engine: "bleve", Mode: dbstore.Persistent, Path: path}
	h, err := blevedb.Adapter{}.Create(ctx, tg, dbstore.Options{})
	if err != nil {
		t.Fatal(err)
	}

	d := h.(*blevedb.DB)
	idx := d.Index()

	if err := idx.SetInternal([]byte("doc:p1"), []byte(`{"text":"world"}`)); err != nil {
		t.Fatalf("SetInternal: %v", err)
	}
	if err := idx.Index("p1", map[string]any{"text": "world"}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	h.Close()

	// Reopen
	h2, err := blevedb.Adapter{}.Open(ctx, tg, dbstore.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	idx2 := h2.(*blevedb.DB).Index()
	val, err := idx2.GetInternal([]byte("doc:p1"))
	if err != nil {
		t.Fatalf("GetInternal: %v", err)
	}
	if string(val) != `{"text":"world"}` {
		t.Fatalf("unexpected val: %s", string(val))
	}
	res, err := idx2.Search(bleve.NewSearchRequest(bleve.NewMatchQuery("world")))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("expected 1 hit, got %d", res.Total)
	}

	dictID, err := idx2.FieldDict("_id")
	if err != nil {
		t.Logf("FieldDict(_id) err: %v", err)
	} else {
		for {
			e, err := dictID.Next()
			if err != nil || e == nil {
				break
			}
			t.Logf("persistent _id term: %s", e.Term)
		}
	}
}
