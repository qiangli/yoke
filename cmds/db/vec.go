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
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbvec"
	"github.com/spf13/pflag"
)

// vecVerb splits `db vec put|get|rm|batch|search|export|import` and
// `db vec collection add|rm|list` (collection-add and the like are the
// one-word spellings typed MCP uses) from the remaining operands.
func vecVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db vec put|get|rm|batch|search|export|import|collection add|rm|list TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if sub == "collection" && len(rest) > 0 {
		sub, rest = "collection."+rest[0], rest[1:]
	} else if v, ok := strings.CutPrefix(sub, "collection-"); ok {
		sub = "collection." + v
	}
	switch sub {
	case "put", "get", "rm", "batch", "search", "export", "import", "collection.add", "collection.rm", "collection.list":
		return "vec." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: db vec %s (want put, get, rm, batch, search, export, import or collection add|rm|list)", dbstore.ErrInvalidInput, operands[0])
}

// vecFlags are the flags `db vec` reads besides the shared target and
// lifecycle ones; any other data flag (--sql, --bucket, --doc, …) is
// invalid_input rather than silently ignored.
var vecFlags = map[string]bool{
	"engine": true, "mode": true, "path": true, "session": true, "name": true, "request-id": true, "attachment": true,
	"epoch": true, "json": true, "yes": true, "dry-run": true, "explain": true, "wait": true, "timeout": true, "input": true,
	"collection": true, "id": true, "filter": true, "params": true, "if-revision": true, "if-absent": true,
	"limit": true, "max-bytes": true, "since": true, "on-conflict": true, "out": true, "in": true, "replace": true,
	"dimension": true, "metric": true, "model": true, "vector": true, "text": true, "endpoint": true,
	"api-key-env": true, "metadata": true, "include-vectors": true,
}

// vecInput builds the one typed vec input from --input and the flags. A
// value given twice is an error, never a silent override.
func vecInput(rc *tool.RunContext, f dbFlags) (json.RawMessage, error) {
	var foreign []string
	// Typed MCP passes every flag, so a flag at its default counts as unset.
	f.fs.Visit(func(fl *pflag.Flag) {
		if !vecFlags[fl.Name] && fl.Value.String() != fl.DefValue {
			foreign = append(foreign, "--"+fl.Name)
		}
	})
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("%w: db vec does not take %s", dbstore.ErrInvalidInput, strings.Join(foreign, ", "))
	}
	var in dbvec.Input
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
			return nil, fmt.Errorf("%w: --input is over %d bytes", dbstore.ErrLimitExceeded, maxInput)
		}
		if in, err = dbvec.DecodeInput(bytes.TrimSpace(b)); err != nil {
			return nil, fmt.Errorf("--input: %w", err)
		}
	}
	twice := func(name string, set bool) error {
		if set {
			return fmt.Errorf("%w: %s given in --input and as a flag", dbstore.ErrInvalidInput, name)
		}
		return nil
	}
	for _, v := range []struct {
		name, flag string
		dst        *string
	}{{"collection", *f.collection, &in.Collection}, {"id", *f.docID, &in.ID}, {"metric", *f.metric, &in.Metric},
		{"model", *f.model, &in.Model}, {"text", *f.text, &in.Text}, {"endpoint", *f.endpoint, &in.Endpoint},
		{"api_key_env", *f.apiKeyEnv, &in.APIKeyEnv}, {"since", *f.since, &in.Since}, {"on_conflict", *f.onConflict, &in.OnConflict}} {
		if v.flag != "" {
			if err := twice(v.name, *v.dst != ""); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	for _, v := range []struct {
		name, flag string
		dst        *json.RawMessage
	}{{"vector", *f.vector, &in.Vector}, {"metadata", *f.metadata, &in.Metadata}, {"filter", *f.filter, &in.Filter}, {"params", *f.params, &in.Params}} {
		if v.flag != "" {
			if err := twice(v.name, len(*v.dst) != 0); err != nil {
				return nil, err
			}
			if !json.Valid([]byte(v.flag)) {
				return nil, fmt.Errorf("%w: --%s is not JSON", dbstore.ErrInvalidInput, v.name)
			}
			*v.dst = json.RawMessage(v.flag)
		}
	}
	for _, v := range []struct {
		name string
		flag int
		dst  *int
	}{{"dimension", *f.dimension, &in.Dimension}, {"limit", *f.limit, &in.Limit}, {"max_bytes", *f.maxBytes, &in.MaxBytes}} {
		if v.flag != 0 {
			if err := twice(v.name, *v.dst != 0); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	if *f.ifRevision != "" {
		if err := twice("if_revision", in.IfRevision != nil); err != nil {
			return nil, err
		}
		r, err := strconv.ParseInt(*f.ifRevision, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: --if-revision wants an integer", dbstore.ErrInvalidInput)
		}
		in.IfRevision = &r
	}
	in.IfAbsent = in.IfAbsent || *f.ifAbsent
	in.IncludeVectors = in.IncludeVectors || *f.includeVectors
	// --out/--in/--replace mean the export directory and import mode here;
	// the operation refuses them anywhere else.
	for _, v := range []struct {
		name, flag string
		dst        *string
	}{{"out", *f.backup, &in.Out}, {"in", *f.source, &in.In}} {
		if v.flag != "" {
			if err := twice(v.name, *v.dst != ""); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	in.Replace = in.Replace || *f.replace
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	return json.Marshal(in)
}
