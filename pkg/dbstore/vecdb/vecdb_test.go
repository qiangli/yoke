package vecdb

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

func TestVectorsBothModes(t *testing.T) {
	ctx := context.Background()
	s, err := dbstore.NewMemSpace(dbstore.LifetimeOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := sqlitedb.Adapter{}
	for mode, tg := range map[string]dbstore.Target{
		"persistent": {Mode: dbstore.Persistent, Path: filepath.Join(t.TempDir(), "v.db")},
		"memory":     {Mode: dbstore.Memory, Session: s.ID(), Name: "v"},
	} {
		t.Run(mode, func(t *testing.T) {
			o := dbstore.Options{Mem: s}
			h, err := a.Create(ctx, tg, o)
			if err != nil {
				t.Fatal(err)
			}
			db := h.(*sqlitedb.DB)
			if v, err := Version(ctx, db); err != nil || v == "" {
				t.Fatalf("vec_version %q %v", v, err)
			}
			if _, err := db.SQL().ExecContext(ctx, "create virtual table e using vec0(id text primary key, emb float[3] distance_metric=cosine)"); err != nil {
				t.Fatal(err)
			}
			// float32 values that do not survive a decimal round trip.
			tiny := float32(math.SmallestNonzeroFloat32)
			for id, v := range map[string][]float32{"a": {1, 0, 0}, "b": {0, 1, 0}, "c": {0.70710677, 0.70710677, tiny}} {
				if _, err := db.SQL().ExecContext(ctx, "insert into e(id, emb) values(?, ?)", id, F32(v)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.SQL().ExecContext(ctx, "insert into e(id, emb) values('bad', ?)", F32([]float32{1, 2})); err == nil {
				t.Fatal("wrong dimension accepted")
			}
			h.Close()
			h, err = a.Open(ctx, tg, dbstore.Options{Mem: s, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			db = h.(*sqlitedb.DB)
			var id string
			var dist float64
			if err := db.SQL().QueryRowContext(ctx, "select id, distance from e where emb match ? and k = 1", F32([]float32{0.9, 0.1, 0})).Scan(&id, &dist); err != nil || id != "a" {
				t.Fatalf("knn %q %v %v", id, dist, err)
			}
			var blob []byte
			if err := db.SQL().QueryRowContext(ctx, "select emb from e where id = 'c'").Scan(&blob); err != nil {
				t.Fatal(err)
			}
			got, err := ParseF32(blob)
			if err != nil || got[2] != tiny || got[0] != 0.70710677 {
				t.Fatalf("lossless embedding %v %v", got, err)
			}
		})
	}
}
