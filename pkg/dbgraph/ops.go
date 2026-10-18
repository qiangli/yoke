package dbgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbstore/bonsaidb"
)

func init() {
	read, write, excl := dblifecycle.AccessRead, dblifecycle.AccessWrite, dblifecycle.AccessExclusive
	for _, row := range []struct {
		name   string
		access dblifecycle.Access
		run    func(context.Context, *bonsaidb.DB, Input) (dblifecycle.Outcome, error)
	}{
		{OpQuery, read, runQuery}, {OpMutate, write, runMutate}, {OpSchemaShow, read, runSchemaShow}, {OpSchemaApply, excl, runSchemaApply},
		{OpNodeGet, read, runNodeGet}, {OpNodePut, write, runNodePut}, {OpNodeRm, write, runNodeRm},
		{OpEdgeGet, read, runEdgeGet}, {OpEdgePut, write, runEdgePut}, {OpEdgeRm, write, runEdgeRm}, {OpEdges, read, runEdges},
		{OpExport, read, runExport}, {OpImport, excl, runImport},
	} {
		register(row.name, row.access, row.run)
	}
}

func register(name string, access dblifecycle.Access, run func(context.Context, *bonsaidb.DB, Input) (dblifecycle.Outcome, error)) {
	dblifecycle.RegisterCategoryOp(name, dblifecycle.CategoryOp{
		Access: func(raw json.RawMessage) (dblifecycle.Access, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return 0, err
			}
			if _, _, err = in.bounds(); err != nil {
				return 0, err
			}
			return access, nil
		},
		Spec: func(mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation { return Spec(name, mode, a) },
		Run: func(ctx context.Context, h dbstore.Handle, raw json.RawMessage, a dblifecycle.Access) (dblifecycle.Outcome, error) {
			in, err := DecodeInput(raw)
			if err != nil {
				return dblifecycle.Outcome{}, err
			}
			if a != access {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: wrong graph access", dbstore.ErrInvalidInput)
			}
			d, ok := h.(*bonsaidb.DB)
			if !ok {
				return dblifecycle.Outcome{}, fmt.Errorf("%w: graph requires bonsai adapter", dbstore.ErrUnsupported)
			}
			return run(ctx, d, in)
		},
	})
}

func Spec(name string, mode dbstore.Mode, a dblifecycle.Access) dblifecycle.Operation {
	s := dblifecycle.Operation{Name: name, MutationClass: "read", Hold: "none", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	switch name {
	case OpQuery, OpMutate, OpSchemaShow, OpSchemaApply:
		s.Modes = []string{"persistent"}
	case OpNodeGet, OpNodePut, OpNodeRm, OpEdgeGet, OpEdgePut, OpEdgeRm, OpEdges, OpExport, OpImport:
		s.Modes = []string{"memory", "persistent"}
	}
	if mode == dbstore.Memory {
		s.Effects = []string{"db.read"}
	} else {
		s.Effects = []string{"fs.read", "db.read"}
	}
	if a != dblifecycle.AccessRead {
		s.MutationClass = "write"
		s.Hold = "shared-writer"
		if mode == dbstore.Memory {
			s.Effects = []string{"db.write"}
		} else {
			s.Effects = []string{"fs.write", "db.write"}
		}
	}
	if a == dblifecycle.AccessExclusive {
		s.MutationClass = "destructive"
		s.Hold = "exclusive"
		s.Effects = append(s.Effects, "db.delete")
	}
	if name == OpExport {
		s.Effects = append(s.Effects, "fs.write")
	}
	if name == OpImport {
		s.Effects = append(s.Effects, "fs.read")
	}
	return s
}

func persistent(d *bonsaidb.DB) error {
	if d.Bonsai() == nil {
		return fmt.Errorf("%w: DQL and schema need a persistent Bonsai target", dbstore.ErrUnsupported)
	}
	return nil
}
func memory(d *bonsaidb.DB) error {
	if d.Bonsai() != nil {
		return fmt.Errorf("%w: typed node/edge operations need the memory graph", dbstore.ErrUnsupported)
	}
	return nil
}

func runQuery(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpQuery, "query", "variables", "max_bytes"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := persistent(d); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: query required", dbstore.ErrInvalidInput)
	}
	_, maxBytes, _ := in.bounds()
	r, err := d.Bonsai().QueryWithVars(ctx, in.Query, in.Variables)
	if err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	if len(r.Json) > maxBytes {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: query result exceeds max_bytes", dbstore.ErrLimitExceeded)
	}
	return dblifecycle.Outcome{Result: json.RawMessage(r.Json)}, nil
}
func runMutate(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpMutate, "set_nquads", "del_nquads"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := persistent(d); err != nil {
		return dblifecycle.Outcome{}, err
	}
	schema, err := d.Bonsai().SchemaText(ctx)
	if err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	if isPortableSchema(schema) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: raw DQL mutations cannot modify the portable typed graph", dbstore.ErrUnsupported)
	}
	if in.SetNQuads == "" && in.DelNQuads == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: set_nquads or del_nquads required", dbstore.ErrInvalidInput)
	}
	r, err := d.Bonsai().Mutate(ctx, &api.Mutation{SetNquads: []byte(in.SetNQuads), DelNquads: []byte(in.DelNQuads), CommitNow: true})
	if err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	return dblifecycle.Outcome{Result: map[string]any{"uids": r.Uids}}, nil
}
func runSchemaShow(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpSchemaShow); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := persistent(d); err != nil {
		return dblifecycle.Outcome{}, err
	}
	s, err := d.Bonsai().SchemaText(ctx)
	return dblifecycle.Outcome{Result: map[string]any{"schema": s}}, dbstore.Backend("bonsai", err)
}
func runSchemaApply(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpSchemaApply, "schema"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if err := persistent(d); err != nil {
		return dblifecycle.Outcome{}, err
	}
	schema, err := d.Bonsai().SchemaText(ctx)
	if err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	if isPortableSchema(schema) {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: raw schema changes cannot modify the portable typed graph", dbstore.ErrUnsupported)
	}
	if strings.TrimSpace(in.Schema) == "" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: schema required", dbstore.ErrInvalidInput)
	}
	if err := d.Bonsai().Alter(ctx, in.Schema); err != nil {
		return dblifecycle.Outcome{}, dbstore.Backend("bonsai", err)
	}
	return dblifecycle.Outcome{Result: map[string]any{"applied": true}}, nil
}

func runNodeGet(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpNodeGet, "id"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentNodeGet(ctx, d, in)
	}
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var n dbstore.Node
	var ok bool
	err := d.View(ctx, func(tx dbstore.GraphTx) (e error) { n, ok, e = tx.GetNode(in.ID); return })
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !ok {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: node %s", dbstore.ErrNotFound, in.ID)
	}
	return dblifecycle.Outcome{Result: n}, nil
}
func runNodePut(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpNodePut, "node", "if_absent", "if_revision"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentNodePut(ctx, d, in)
	}
	if in.Node == nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: node required", dbstore.ErrInvalidInput)
	}
	if err := checkID(in.Node.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	var rev uint64
	err := d.Update(ctx, func(tx dbstore.GraphTx) (e error) {
		rev, e = tx.PutNode(*in.Node, dbstore.Cond{IfAbsent: in.IfAbsent, IfRev: in.IfRevision})
		return
	})
	return dblifecycle.Outcome{Result: map[string]any{"id": in.Node.ID, "revision": rev}}, err
}
func runNodeRm(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpNodeRm, "id", "if_revision"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentNodeRm(ctx, d, in)
	}
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	err := d.Update(ctx, func(tx dbstore.GraphTx) error {
		n, ok, e := tx.GetNode(in.ID)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("%w: node %s", dbstore.ErrNotFound, in.ID)
		}
		if in.IfRevision != 0 && n.Rev != in.IfRevision {
			return fmt.Errorf("%w: node revision %d, expected %d", dbstore.ErrConflict, n.Rev, in.IfRevision)
		}
		return tx.DeleteNode(in.ID)
	})
	return dblifecycle.Outcome{Result: map[string]any{"deleted": in.ID}}, err
}
func runEdgeGet(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpEdgeGet, "from", "type", "to"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentEdgeGet(ctx, d, in)
	}
	var e dbstore.Edge
	var ok bool
	err := d.View(ctx, func(tx dbstore.GraphTx) (x error) { e, ok, x = tx.GetEdge(in.From, in.Type, in.To); return })
	if err != nil {
		return dblifecycle.Outcome{}, err
	}
	if !ok {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: edge", dbstore.ErrNotFound)
	}
	return dblifecycle.Outcome{Result: e}, nil
}
func runEdgePut(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpEdgePut, "edge", "if_absent", "if_revision"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentEdgePut(ctx, d, in)
	}
	if in.Edge == nil {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: edge required", dbstore.ErrInvalidInput)
	}
	var rev uint64
	err := d.Update(ctx, func(tx dbstore.GraphTx) (e error) {
		rev, e = tx.PutEdge(*in.Edge, dbstore.Cond{IfAbsent: in.IfAbsent, IfRev: in.IfRevision})
		return
	})
	return dblifecycle.Outcome{Result: map[string]any{"revision": rev}}, err
}
func runEdgeRm(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpEdgeRm, "from", "type", "to", "if_revision"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentEdgeRm(ctx, d, in)
	}
	err := d.Update(ctx, func(tx dbstore.GraphTx) error {
		e, ok, x := tx.GetEdge(in.From, in.Type, in.To)
		if x != nil {
			return x
		}
		if !ok {
			return fmt.Errorf("%w: edge", dbstore.ErrNotFound)
		}
		if in.IfRevision != 0 && e.Rev != in.IfRevision {
			return fmt.Errorf("%w: edge revision %d, expected %d", dbstore.ErrConflict, e.Rev, in.IfRevision)
		}
		return tx.DeleteEdge(in.From, in.Type, in.To)
	})
	return dblifecycle.Outcome{Result: map[string]any{"deleted": true}}, err
}
func runEdges(ctx context.Context, d *bonsaidb.DB, in Input) (dblifecycle.Outcome, error) {
	if err := in.only(OpEdges, "id", "type", "direction", "limit", "max_bytes"); err != nil {
		return dblifecycle.Outcome{}, err
	}
	if d.Bonsai() != nil {
		return persistentEdges(ctx, d, in)
	}
	if err := checkID(in.ID); err != nil {
		return dblifecycle.Outcome{}, err
	}
	limit, maxBytes, _ := in.bounds()
	dir := dbstore.Out
	if in.Direction == "in" {
		dir = dbstore.In
	} else if in.Direction != "" && in.Direction != "out" {
		return dblifecycle.Outcome{}, fmt.Errorf("%w: direction in|out", dbstore.ErrInvalidInput)
	}
	es := []dbstore.Edge{}
	used := 0
	truncated := false
	err := d.View(ctx, func(tx dbstore.GraphTx) error {
		return tx.Edges(in.ID, dir, in.Type, func(e dbstore.Edge) (bool, error) {
			b, _ := json.Marshal(e)
			if len(b) > maxBytes {
				return false, fmt.Errorf("%w: edge exceeds max_bytes", dbstore.ErrLimitExceeded)
			}
			if len(es) >= limit || used+len(b) > maxBytes {
				truncated = true
				return false, nil
			}
			es = append(es, e)
			used += len(b)
			return true, nil
		})
	})
	return dblifecycle.Outcome{Result: map[string]any{"edges": es, "truncated": truncated}}, err
}
