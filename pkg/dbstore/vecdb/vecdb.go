// Package vecdb selects the vector backend for the Sprint 453 surface:
// sqlite-vec's vec0 virtual table, compiled CGO-free by the pinned
// modernc.org/sqlite module (modernc.org/sqlite/vec, sqlite-vec v0.1.9 as of
// modernc v1.50.0). It adds no engine of its own: vectors live in a sqlitedb
// database, so both storage modes, sharing and locking are sqlitedb's.
//
// Importing this package registers vec0 as a SQLite auto-extension for every
// modernc connection in the process. sqlite-vec is pre-v1; its on-disk format
// may change between upstream releases, which is one reason logical export
// (not file copy) is the interchange path.
package vecdb

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	_ "modernc.org/sqlite/vec"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/sqlitedb"
)

// Caps is the sqlite adapter's capabilities with the vec category added.
func Caps() dbstore.Caps {
	c := sqlitedb.Adapter{}.Caps()
	c.Categories = append(c.Categories, "vec")
	c.Notes = append(c.Notes, "vectors use sqlite-vec vec0 (pre-v1, CGO-free via modernc.org/sqlite/vec); exact KNN, no ANN index")
	return c
}

// Version reports the linked sqlite-vec version, proving the extension is
// available on this connection.
func Version(ctx context.Context, db *sqlitedb.DB) (string, error) {
	var v string
	if err := db.SQL().QueryRowContext(ctx, "select vec_version()").Scan(&v); err != nil {
		return "", sqlitedb.ClassifyCtx(ctx, err)
	}
	return v, nil
}

// F32 encodes a vector as sqlite-vec's compact little-endian float32 blob,
// which is lossless for float32 embeddings (JSON text is not).
func F32(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// ParseF32 decodes a float32 blob.
func ParseF32(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("%w: float32 blob length %d", dbstore.ErrInvalidInput, len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, nil
}
