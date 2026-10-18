package dbgraph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
)

// These predicates are a deliberately narrow portable island in Bonsai.
// Native DQL data remains available through query/mutate and native backups;
// logical export refuses a database with any other user predicates.
const portableSchema = "bashy_graph_key: string @index(exact) .\nbashy_graph_data: string ."

type nativeRow struct {
	UID  string `json:"uid"`
	Key  string `json:"bashy_graph_key"`
	Data string `json:"bashy_graph_data"`
}

func nodeKey(id string) string { return "n:" + base64.RawURLEncoding.EncodeToString([]byte(id)) }
func edgeKey(e dbstore.Edge) string {
	return "e:" + base64.RawURLEncoding.EncodeToString([]byte(e.From)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(e.Type)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(e.To))
}

func nativeRows(ctx context.Context, d *bonsaidb.DB) ([]nativeRow, error) {
	if err := checkPortableSchema(ctx, d); err != nil {
		return nil, err
	}
	r, err := d.Bonsai().Query(ctx, `{ q(func: has(bashy_graph_key), first: 100001) { uid bashy_graph_key bashy_graph_data } }`)
	if err != nil {
		return nil, dbstore.Backend("bonsai", err)
	}
	if len(r.Json) > 64<<20 {
		return nil, dbstore.ErrLimitExceeded
	}
	var out struct {
		Q []nativeRow `json:"q"`
	}
	if err := json.Unmarshal(r.Json, &out); err != nil {
		return nil, fmt.Errorf("%w: portable graph result: %v", dbstore.ErrIntegrity, err)
	}
	if len(out.Q) > graphMaxRecords {
		return nil, dbstore.ErrLimitExceeded
	}
	sort.Slice(out.Q, func(i, j int) bool { return out.Q[i].Key < out.Q[j].Key })
	for i, row := range out.Q {
		if row.Key == "" || row.Data == "" || (i > 0 && out.Q[i-1].Key == row.Key) {
			return nil, fmt.Errorf("%w: ambiguous portable graph row", dbstore.ErrIntegrity)
		}
	}
	return out.Q, nil
}

func checkPortableSchema(ctx context.Context, d *bonsaidb.DB) error {
	s, err := d.Bonsai().SchemaText(ctx)
	if err != nil {
		return dbstore.Backend("bonsai", err)
	}
	if strings.TrimSpace(s) != "" && !isPortableSchema(s) {
		return fmt.Errorf("%w: graph has arbitrary DQL schema; use native backup", dbstore.ErrUnsupported)
	}
	return nil
}

func isPortableSchema(s string) bool {
	parts := strings.Split(strings.TrimSpace(s), "\n")
	if len(parts) != 2 {
		return false
	}
	a, b := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	return (a == "bashy_graph_key: string @index(exact) ." && b == "bashy_graph_data: string .") ||
		(a == "bashy_graph_data: string ." && b == "bashy_graph_key: string @index(exact) .")
}

func findRow(rows []nativeRow, key string) (nativeRow, bool) {
	i := sort.Search(len(rows), func(i int) bool { return rows[i].Key >= key })
	if i < len(rows) && rows[i].Key == key {
		return rows[i], true
	}
	return nativeRow{}, false
}

func decodeNode(row nativeRow) (dbstore.Node, error) {
	var n dbstore.Node
	b, err := base64.StdEncoding.DecodeString(row.Data)
	if err == nil {
		err = json.Unmarshal(b, &n)
	}
	if err != nil || nodeKey(n.ID) != row.Key {
		return n, fmt.Errorf("%w: malformed portable node %q", dbstore.ErrIntegrity, row.Key)
	}
	return n, nil
}
func decodeEdge(row nativeRow) (dbstore.Edge, error) {
	var e dbstore.Edge
	b, err := base64.StdEncoding.DecodeString(row.Data)
	if err == nil {
		err = json.Unmarshal(b, &e)
	}
	if err != nil || edgeKey(e) != row.Key {
		return e, fmt.Errorf("%w: malformed portable edge %q", dbstore.ErrIntegrity, row.Key)
	}
	return e, nil
}

func putNative(ctx context.Context, d *bonsaidb.DB, key string, value any, old nativeRow) error {
	if old.UID == "" {
		if err := d.Bonsai().Alter(ctx, portableSchema); err != nil {
			return dbstore.Backend("bonsai", err)
		}
		old.UID = "_:graph"
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	set, _ := json.Marshal(map[string]string{"uid": old.UID, "bashy_graph_key": key, "bashy_graph_data": base64.StdEncoding.EncodeToString(b)})
	_, err = d.Bonsai().Mutate(ctx, &api.Mutation{SetJson: set, CommitNow: true})
	return dbstore.Backend("bonsai", err)
}

func deleteNative(ctx context.Context, d *bonsaidb.DB, rows ...nativeRow) error {
	if len(rows) == 0 {
		return nil
	}
	var del strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&del, "<%s> <bashy_graph_key> %q .\n<%s> <bashy_graph_data> %q .\n", row.UID, row.Key, row.UID, row.Data)
	}
	_, err := d.Bonsai().Mutate(ctx, &api.Mutation{DelNquads: []byte(del.String()), CommitNow: true})
	return dbstore.Backend("bonsai", err)
}

func conflict(found bool, rev uint64, in Input) error {
	if in.IfAbsent && found {
		return fmt.Errorf("%w: graph item exists", dbstore.ErrConflict)
	}
	if in.IfRevision != 0 && (!found || rev != in.IfRevision) {
		return fmt.Errorf("%w: graph revision differs", dbstore.ErrConflict)
	}
	return nil
}

func persistentNodeGet(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	row, ok := findRow(rows, nodeKey(in.ID))
	if !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	n, err := decodeNode(row)
	return dblifecycle.Outcome{Result: n}, err
}
func persistentNodePut(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if in.Node == nil {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	if err := checkID(in.Node.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	key := nodeKey(in.Node.ID)
	row, ok := findRow(rows, key)
	var rev uint64
	if ok {
		n, err := decodeNode(row)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		rev = n.Rev
	}
	if err := conflict(ok, rev, in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	n := *in.Node
	n.Rev = rev + 1
	if err := putNative(ctx, d, key, n, row); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"id": n.ID, "revision": n.Rev}}, nil
}
func persistentNodeRm(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	row, ok := findRow(rows, nodeKey(in.ID))
	if !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	n, err := decodeNode(row)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := conflict(true, n.Rev, in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	deleted := []nativeRow{row}
	for _, r := range rows {
		if strings.HasPrefix(r.Key, "e:") {
			e, err := decodeEdge(r)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if e.From == in.ID || e.To == in.ID {
				deleted = append(deleted, r)
			}
		}
	}
	if err := deleteNative(ctx, d, deleted...); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"deleted": in.ID}}, nil
}
func persistentEdgeGet(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	row, ok := findRow(rows, edgeKey(dbstore.Edge{From: in.From, Type: in.Type, To: in.To}))
	if !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	e, err := decodeEdge(row)
	return dblifecycle.Outcome{Result: e}, err
}
func persistentEdgePut(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if in.Edge == nil {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	e := *in.Edge
	for _, s := range []string{e.From, e.Type, e.To} {
		if err := checkID(s); err != nil {
			return dblifecycle.Outcome{}, err
		}
	}
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if _, ok := findRow(rows, nodeKey(e.From)); !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	if _, ok := findRow(rows, nodeKey(e.To)); !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	key := edgeKey(e)
	row, ok := findRow(rows, key)
	var rev uint64
	if ok {
		cur, err := decodeEdge(row)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		rev = cur.Rev
	}
	if err := conflict(ok, rev, in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	e.Rev = rev + 1
	if err := putNative(ctx, d, key, e, row); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"revision": e.Rev}}, nil
}
func persistentEdgeRm(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	row, ok := findRow(rows, edgeKey(dbstore.Edge{From: in.From, Type: in.Type, To: in.To}))
	if !ok {
		return dblifecycle.Outcome{}, dbstore.ErrNotFound
	}
	e, err := decodeEdge(row)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := conflict(true, e.Rev, in); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := deleteNative(ctx, d, row); err != nil {
		return dblifecycle.Outcome{}, err
	}
	return dblifecycle.Outcome{Result: map[string]any{"deleted": true}}, nil
}
func persistentEdges(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, err := in.bounds()
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if in.Direction != "" && in.Direction != "in" && in.Direction != "out" {
		return dblifecycle.Outcome{}, dbstore.ErrInvalidInput
	}
	rows, err := nativeRows(ctx, d)
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	es := []dbstore.Edge{}
	used := 0
	truncated := false
	for _, r := range rows {
		if !strings.HasPrefix(r.Key, "e:") {
			continue
		}
		e, err := decodeEdge(r)
		if err != nil {
			return dblifecycle.Outcome{}, err
		}
		if in.Direction == "in" && e.To != in.ID || in.Direction != "in" && e.From != in.ID || in.Type != "" && e.Type != in.Type {
			continue
		}
		b, _ := json.Marshal(e)
		if len(b) > maxBytes {
			return dblifecycle.Outcome{}, dbstore.ErrLimitExceeded
		}
		if len(es) >= limit || used+len(b) > maxBytes {
			truncated = true
			break
		}
		es = append(es, e)
		used += len(b)
	}
	return dblifecycle.Outcome{Result: map[string]any{"edges": es, "truncated": truncated}}, nil
}
