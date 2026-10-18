package dbgraph

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
)

const graphExportSchema = "bashy-db-export-v1"
const graphFormatVersion = "1.0"
const graphRecordLimit = 8 << 20
const graphManifestLimit = 1 << 20
const graphMaxRecords = 100000

type graphManifest struct {
	SchemaVersion string `json:"schema_version"`
	FormatVersion string `json:"format_version"`
	Category      string `json:"category"`
	Engine        string `json:"engine"`
	Source        struct {
		ID   string `json:"id"`
		Mode string `json:"mode"`
	} `json:"source"`
	Snapshot struct {
		Consistent bool   `json:"consistent"`
		Method     string `json:"method"`
	} `json:"snapshot"`
	SchemaDef struct {
		Representation string `json:"representation"`
		Predicates     string `json:"predicates"`
	} `json:"schema_def"`
	Files []graphFile `json:"files"`
}
type graphFile struct {
	Path    string `json:"path"`
	Records int    `json:"records"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
}
type graphRecord struct {
	Kind string        `json:"kind"`
	Node *dbstore.Node `json:"node,omitempty"`
	Edge *dbstore.Edge `json:"edge,omitempty"`
}

// portableRows refuses an arbitrary DQL schema. Native backup is the fidelity
// path for DQL outside these reserved predicates.
func portableRows(ctx context.Context, d *bonsaidb.DB) ([]graphRecord, error) {
	if d.Bonsai() != nil {
		s, err := d.Bonsai().SchemaText(ctx)
		if err != nil {
			return nil, dbstore.Backend("bonsai", err)
		}
		if strings.TrimSpace(s) != "" && !isPortableSchema(s) {
			return nil, fmt.Errorf("%w: arbitrary DQL schema requires native backup", dbstore.ErrUnsupported)
		}
		rows, err := nativeRows(ctx, d)
		if err != nil {
			return nil, err
		}
		if len(rows) > graphMaxRecords {
			return nil, dbstore.ErrLimitExceeded
		}
		out := make([]graphRecord, 0, len(rows))
		for _, r := range rows {
			if err := ctx.Err(); err != nil {
				return nil, dbstore.FromContext(err)
			}
			switch {
			case strings.HasPrefix(r.Key, "n:"):
				n, e := decodeNode(r)
				if e != nil {
					return nil, e
				}
				out = append(out, graphRecord{Kind: "node", Node: &n})
			case strings.HasPrefix(r.Key, "e:"):
				e2, e := decodeEdge(r)
				if e != nil {
					return nil, e
				}
				out = append(out, graphRecord{Kind: "edge", Edge: &e2})
			default:
				return nil, fmt.Errorf("%w: external key %q", dbstore.ErrUnsupported, r.Key)
			}
		}
		return out, nil
	}
	out := []graphRecord{}
	err := d.View(ctx, func(tx dbstore.GraphTx) error {
		return tx.Nodes(func(n dbstore.Node) (bool, error) {
			if err := ctx.Err(); err != nil {
				return false, dbstore.FromContext(err)
			}
			if len(out) >= graphMaxRecords {
				return false, dbstore.ErrLimitExceeded
			}
			copyN := n
			out = append(out, graphRecord{Kind: "node", Node: &copyN})
			return true, tx.Edges(n.ID, dbstore.Out, "", func(e dbstore.Edge) (bool, error) {
				if len(out) >= graphMaxRecords {
					return false, dbstore.ErrLimitExceeded
				}
				copyE := e
				out = append(out, graphRecord{Kind: "edge", Edge: &copyE})
				return true, nil
			})
		})
	})
	return out, err
}

func runExport(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpExport, "out"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !filepath.IsAbs(in.Out) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: export requires absolute out directory", dbstore.ErrInvalidInput)
	}
	rows, err := portableRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sort.Slice(rows, func(i, j int) bool { return recordKey(rows[i]) < recordKey(rows[j]) })
	if err := os.Mkdir(in.Out, 0700); err != nil {
		if os.IsExist(err) {
			return dblifecycle.Outcome{}, dbstore.ErrExists
		}
		return dblifecycle.Outcome{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(in.Out)
		}
	}()
	f, err := os.OpenFile(filepath.Join(in.Out, "records.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	sum := sha256.New()
	w := bufio.NewWriter(io.MultiWriter(f, sum))
	var size int64
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			f.Close()
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		b, err := json.Marshal(row)
		if err != nil {
			f.Close()
			return dblifecycle.Outcome{}, err
		}
		if len(b)+1 > graphRecordLimit {
			f.Close()
			return dblifecycle.Outcome{}, dbstore.ErrLimitExceeded
		}
		b = append(b, '\n')
		n, err := w.Write(b)
		size += int64(n)
		if err != nil {
			f.Close()
			return dblifecycle.Outcome{}, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return dblifecycle.Outcome{}, err
	}
	if err := f.Close(); err != nil {
		return dblifecycle.Outcome{}, err
	}
	m := graphManifest{SchemaVersion: graphExportSchema, FormatVersion: graphFormatVersion, Category: "graph", Engine: "bonsai"}
	id := d.Identity()
	m.Source.ID = id.ID
	m.Source.Mode = string(id.Mode)
	m.Snapshot.Consistent = true
	m.Snapshot.Method = "read-txn"
	if d.Bonsai() != nil {
		m.Snapshot.Method = "bonsai-query-snapshot"
	}
	m.SchemaDef.Representation = "bashy-typed-graph-v1"
	m.SchemaDef.Predicates = portableSchema
	m.Files = []graphFile{{"records.jsonl", len(rows), size, hex.EncodeToString(sum.Sum(nil))}}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := os.WriteFile(filepath.Join(in.Out, "manifest.json"), append(b, '\n'), 0600); err != nil {
		return dblifecycle.Outcome{}, err
	}
	ok = true
	return dblifecycle.Outcome{Result: map[string]any{"out": in.Out, "manifest": "manifest.json", "format_version": graphFormatVersion, "records": len(rows), "snapshot": m.Snapshot}}, nil
}
func recordKey(r graphRecord) string {
	if r.Node != nil {
		return nodeKey(r.Node.ID)
	}
	if r.Edge != nil {
		return edgeKey(*r.Edge)
	}
	return ""
}

func readGraphExport(ctx context.Context, dir string) ([]graphRecord, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%w: import requires absolute directory", dbstore.ErrInvalidInput)
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", dbstore.ErrMalformedInput, err)
	}
	if len(b) > graphManifestLimit {
		return nil, dbstore.ErrLimitExceeded
	}
	var m graphManifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", dbstore.ErrMalformedInput, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, dbstore.ErrMalformedInput
	}
	if m.SchemaVersion != graphExportSchema || !strings.HasPrefix(m.FormatVersion, "1.") {
		return nil, dbstore.ErrVersionUnsupported
	}
	if m.Category != "graph" || m.SchemaDef.Representation != "bashy-typed-graph-v1" || m.SchemaDef.Predicates != portableSchema || len(m.Files) != 1 || m.Files[0].Path != "records.jsonl" {
		return nil, dbstore.ErrUnsupported
	}
	entry := m.Files[0]
	if entry.Records < 0 || entry.Records > graphMaxRecords || entry.Bytes < 0 {
		return nil, dbstore.ErrLimitExceeded
	}
	f, err := os.Open(filepath.Join(dir, entry.Path))
	if err != nil {
		return nil, fmt.Errorf("%w: records: %v", dbstore.ErrMalformedInput, err)
	}
	defer f.Close()
	sum := sha256.New()
	r := bufio.NewReader(io.TeeReader(f, sum))
	out := make([]graphRecord, 0, entry.Records)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, dbstore.FromContext(err)
		}
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			size += int64(len(line))
			if len(line) > graphRecordLimit || len(out) >= graphMaxRecords {
				return nil, dbstore.ErrLimitExceeded
			}
			var rec graphRecord
			d := json.NewDecoder(bytes.NewReader(line))
			d.DisallowUnknownFields()
			if e := d.Decode(&rec); e != nil {
				return nil, fmt.Errorf("%w: record: %v", dbstore.ErrMalformedInput, e)
			}
			if _, e := d.Token(); e != io.EOF {
				return nil, dbstore.ErrMalformedInput
			}
			out = append(out, rec)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if size != entry.Bytes || len(out) != entry.Records || hex.EncodeToString(sum.Sum(nil)) != entry.SHA256 {
		return nil, dbstore.ErrIntegrity
	}
	seen := map[string]bool{}
	nodes := map[string]bool{}
	for _, rec := range out {
		var key string
		switch rec.Kind {
		case "node":
			if rec.Node == nil || rec.Edge != nil || rec.Node.Rev == 0 {
				return nil, dbstore.ErrMalformedInput
			}
			if err := checkID(rec.Node.ID); err != nil {
				return nil, err
			}
			key = nodeKey(rec.Node.ID)
			nodes[rec.Node.ID] = true
		case "edge":
			if rec.Edge == nil || rec.Node != nil || rec.Edge.Rev == 0 {
				return nil, dbstore.ErrMalformedInput
			}
			for _, s := range []string{rec.Edge.From, rec.Edge.To, rec.Edge.Type} {
				if err := checkID(s); err != nil {
					return nil, err
				}
			}
			key = edgeKey(*rec.Edge)
		default:
			return nil, dbstore.ErrMalformedInput
		}
		if seen[key] {
			return nil, dbstore.ErrMalformedInput
		}
		seen[key] = true
	}
	for _, rec := range out {
		if rec.Edge != nil && (!nodes[rec.Edge.From] || !nodes[rec.Edge.To]) {
			return nil, fmt.Errorf("%w: edge endpoint absent from export", dbstore.ErrMalformedInput)
		}
	}
	return out, nil
}

func runImport(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpImport, "in", "on_conflict", "replace"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	policy := in.OnConflict
	if policy == "" {
		policy = "error"
	}
	if policy != "error" && policy != "skip" && policy != "replace" {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	if in.Replace && policy != "error" {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	rows, err := readGraphExport(ctx, in.In)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return importPersistent(ctx, d, in, policy, rows)
	}
	var applied, skipped int
	err = d.Update(ctx, func(tx dbstore.GraphTx) error {
		if in.Replace {
			var ids []string
			if err := tx.Nodes(func(n dbstore.Node) (bool, error) { ids = append(ids, n.ID); return true, nil }); err != nil {
				return err
			}
			for _, id := range ids {
				if err := tx.DeleteNode(id); err != nil {
					return err
				}
			}
		}
		for pass := 0; pass < 2; pass++ {
			for _, rec := range rows {
				if pass == 0 && rec.Node == nil || pass == 1 && rec.Edge == nil {
					continue
				}
				var exists, same bool
				if rec.Node != nil {
					old, ok, e := tx.GetNode(rec.Node.ID)
					if e != nil {
						return e
					}
					exists = ok
					same = ok && old.Type == rec.Node.Type && bytes.Equal(old.Props, rec.Node.Props) && old.Rev == rec.Node.Rev
				} else {
					old, ok, e := tx.GetEdge(rec.Edge.From, rec.Edge.Type, rec.Edge.To)
					if e != nil {
						return e
					}
					exists = ok
					same = ok && bytes.Equal(old.Props, rec.Edge.Props) && old.Rev == rec.Edge.Rev
				}
				if exists {
					if same || policy == "skip" {
						skipped++
						continue
					}
					if policy == "error" {
						return dbstore.ErrConflict
					}
				}
				if rec.Node != nil {
					if e := tx.ImportNode(*rec.Node); e != nil {
						return e
					}
				} else {
					if e := tx.ImportEdge(*rec.Edge); e != nil {
						return e
					}
				}
				applied++
			}
		}
		return nil
	})
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"applied": applied, "skipped": skipped, "atomic": true}}, nil
}

func importPersistent(ctx context.Context, d *bonsaidb.DB, in Input, policy string, records []graphRecord) (dblifecycle.Outcome, error) {
	current, err := portableRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	oldRows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	old := map[string]nativeRow{}
	for _, r := range oldRows {
		old[r.Key] = r
	}
	prev := map[string]graphRecord{}
	for _, r := range current {
		prev[recordKey(r)] = r
	}
	selected := []graphRecord{}
	skipped := 0
	for _, r := range records {
		key := recordKey(r)
		if existing, ok := prev[key]; ok && !in.Replace {
			a, _ := json.Marshal(existing)
			b, _ := json.Marshal(r)
			if bytes.Equal(a, b) || policy == "skip" {
				skipped++
				continue
			}
			if policy == "error" {
				return dblifecycle.Outcome{}, dbstore.ErrConflict
			}
		}
		selected = append(selected, r)
	}
	if len(selected) == 0 && !in.Replace {
		return dblifecycle.Outcome{Result: map[string]any{"applied": 0, "skipped": skipped, "atomic": true}}, nil
	}
	if err := d.Bonsai().Alter(ctx, portableSchema); err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	var del strings.Builder
	if in.Replace {
		for _, row := range oldRows {
			fmt.Fprintf(&del, "<%s> <bashy_graph_key> %q .\n<%s> <bashy_graph_data> %q .\n", row.UID, row.Key, row.UID, row.Data)
		}
	}
	set := []map[string]string{}
	for i, r := range selected {
		if err := ctx.Err(); err != nil {
			return dblifecycle.Outcome{}, dbstore.FromContext(err)
		}
		key := recordKey(r)
		uid := fmt.Sprintf("_:record%d", i)
		if oldRow, ok := old[key]; ok && !in.Replace {
			uid = oldRow.UID
		}
		var v any
		if r.Node != nil {
			v = r.Node
		} else {
			v = r.Edge
		}
		b, _ := json.Marshal(v)
		set = append(set, map[string]string{"uid": uid, "bashy_graph_key": key, "bashy_graph_data": base64.StdEncoding.EncodeToString(b)})
	}
	b, _ := json.Marshal(set)
	_, err = d.Bonsai().Mutate(ctx, &api.Mutation{SetJson: b, DelNquads: []byte(del.String()), CommitNow: true})
	if err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	return dblifecycle.Outcome{Result: map[string]any{"applied": len(selected), "skipped": skipped, "atomic": true}}, nil
}
