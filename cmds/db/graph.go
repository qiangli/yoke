package dbcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dbgraph"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/spf13/pflag"
)

func graphVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db graph VERB TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if (sub == "schema" || sub == "node" || sub == "edge") && len(rest) > 0 {
		sub, rest = sub+"."+rest[0], rest[1:]
	} else {
		for _, prefix := range []string{"schema-", "node-", "edge-"} {
			if suffix, ok := strings.CutPrefix(sub, prefix); ok {
				sub = strings.TrimSuffix(prefix, "-") + "." + suffix
				break
			}
		}
	}
	switch sub {
	case "query", "mutate", "schema.show", "schema.apply", "node.get", "node.put", "node.rm", "edge.get", "edge.put", "edge.rm", "edges", "export", "import":
		return "graph." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: unknown graph verb %s", dbstore.ErrInvalidInput, sub)
}

var graphFlags = map[string]bool{
	"engine": true, "mode": true, "path": true, "session": true, "name": true,
	"request-id": true, "attachment": true, "epoch": true, "json": true,
	"yes": true, "dry-run": true, "explain": true, "wait": true, "timeout": true,
	"input": true, "query": true, "variables": true, "set-nquads": true,
	"del-nquads": true, "schema": true, "node": true, "edge": true,
	"id": true, "from": true, "type": true, "to": true, "direction": true,
	"if-absent": true, "if-revision": true, "limit": true, "max-bytes": true,
	"out": true, "in": true, "on-conflict": true, "replace": true,
}

func graphInput(rc *tool.RunContext, f dbFlags) (json.RawMessage, error) {
	var foreign []string
	f.fs.Visit(func(fl *pflag.Flag) {
		if !graphFlags[fl.Name] && fl.Value.String() != fl.DefValue {
			foreign = append(foreign, "--"+fl.Name)
		}
	})
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("%w: db graph does not take %s", dbstore.ErrInvalidInput, strings.Join(foreign, ", "))
	}
	var in dbgraph.Input
	if *f.input != "" {
		var r io.Reader = rc.In
		if *f.input != "-" {
			file, err := os.Open(resolvePath(rc, *f.input))
			if err != nil {
				return nil, fmt.Errorf("%w: --input: %v", dbstore.ErrInvalidInput, err)
			}
			defer file.Close()
			r = file
		}
		if r == nil {
			return nil, fmt.Errorf("%w: --input - needs stdin", dbstore.ErrInvalidInput)
		}
		b, err := io.ReadAll(io.LimitReader(r, maxInput+1))
		if err != nil {
			return nil, err
		}
		if len(b) > maxInput {
			return nil, dbstore.ErrLimitExceeded
		}
		in, err = dbgraph.DecodeInput(bytes.TrimSpace(b))
		if err != nil {
			return nil, err
		}
	}
	str := func(name, value string, dst *string) error {
		if value == "" {
			return nil
		}
		if *dst != "" {
			return fmt.Errorf("%w: duplicate %s", dbstore.ErrInvalidInput, name)
		}
		*dst = value
		return nil
	}
	for _, item := range []struct {
		name, value string
		dst         *string
	}{
		{"query", *f.graphQuery, &in.Query}, {"set_nquads", *f.graphSet, &in.SetNQuads},
		{"del_nquads", *f.graphDel, &in.DelNQuads}, {"schema", *f.graphSchema, &in.Schema},
		{"id", *f.docID, &in.ID}, {"from", *f.graphFrom, &in.From},
		{"type", *f.graphType, &in.Type}, {"to", *f.graphTo, &in.To},
		{"direction", *f.graphDirection, &in.Direction}, {"out", *f.backup, &in.Out},
		{"in", *f.source, &in.In}, {"on_conflict", *f.onConflict, &in.OnConflict},
	} {
		if err := str(item.name, item.value, item.dst); err != nil {
			return nil, err
		}
	}
	if *f.graphVariables != "" {
		if in.Variables != nil {
			return nil, fmt.Errorf("%w: duplicate variables", dbstore.ErrInvalidInput)
		}
		if err := json.Unmarshal([]byte(*f.graphVariables), &in.Variables); err != nil || in.Variables == nil {
			return nil, fmt.Errorf("%w: --variables wants a JSON object of strings", dbstore.ErrInvalidInput)
		}
	}
	if *f.graphNode != "" {
		if in.Node != nil {
			return nil, fmt.Errorf("%w: duplicate node", dbstore.ErrInvalidInput)
		}
		if err := json.Unmarshal([]byte(*f.graphNode), &in.Node); err != nil || in.Node == nil {
			return nil, fmt.Errorf("%w: --node wants typed node JSON", dbstore.ErrInvalidInput)
		}
	}
	if *f.graphEdge != "" {
		if in.Edge != nil {
			return nil, fmt.Errorf("%w: duplicate edge", dbstore.ErrInvalidInput)
		}
		if err := json.Unmarshal([]byte(*f.graphEdge), &in.Edge); err != nil || in.Edge == nil {
			return nil, fmt.Errorf("%w: --edge wants typed edge JSON", dbstore.ErrInvalidInput)
		}
	}
	if *f.ifRevision != "" {
		if in.IfRevision != 0 {
			return nil, fmt.Errorf("%w: duplicate if_revision", dbstore.ErrInvalidInput)
		}
		r, err := strconv.ParseUint(*f.ifRevision, 10, 64)
		if err != nil || r == 0 {
			return nil, fmt.Errorf("%w: --if-revision wants a positive integer", dbstore.ErrInvalidInput)
		}
		in.IfRevision = r
	}
	if *f.ifAbsent && in.IfAbsent {
		return nil, fmt.Errorf("%w: duplicate if_absent", dbstore.ErrInvalidInput)
	}
	in.IfAbsent = in.IfAbsent || *f.ifAbsent
	if *f.replace && in.Replace {
		return nil, fmt.Errorf("%w: duplicate replace", dbstore.ErrInvalidInput)
	}
	in.Replace = in.Replace || *f.replace
	if *f.limit != 0 {
		if in.Limit != 0 {
			return nil, fmt.Errorf("%w: duplicate limit", dbstore.ErrInvalidInput)
		}
		in.Limit = *f.limit
	}
	if *f.maxBytes != 0 {
		if in.MaxBytes != 0 {
			return nil, fmt.Errorf("%w: duplicate max_bytes", dbstore.ErrInvalidInput)
		}
		in.MaxBytes = *f.maxBytes
	}
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	return json.Marshal(in)
}
