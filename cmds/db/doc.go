package dbcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dbdoc"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// docVerb splits `db doc get|find|put|patch|rm|batch|export|import`,
// `db doc collection create|drop|list` and `db doc index add|rm|list`
// (collection-create and the like are the one-word spellings typed MCP
// uses) from the remaining operands.
func docVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db doc get|find|put|patch|rm|batch|export|import|collection …|index … TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if (sub == "collection" || sub == "index") && len(rest) > 0 {
		sub, rest = sub+"."+rest[0], rest[1:]
	} else if group, v, ok := strings.Cut(sub, "-"); ok && (group == "collection" || group == "index") {
		sub = group + "." + v
	}
	switch sub {
	case "get", "find", "put", "patch", "rm", "batch", "export", "import",
		"collection.create", "collection.drop", "collection.list", "index.add", "index.rm", "index.list":
		return "doc." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: db doc %s (want get, find, put, patch, rm, batch, export, import, collection create|drop|list or index add|rm|list)", dbstore.ErrInvalidInput, operands[0])
}

// docInput builds the one typed doc input from --input and the flags. A
// value given twice is an error, never a silent override.
func docInput(rc *tool.RunContext, f dbFlags, catOp string) (json.RawMessage, error) {
	var in dbdoc.Input
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
		if in, err = dbdoc.DecodeInput(bytes.TrimSpace(b)); err != nil {
			return nil, fmt.Errorf("--input: %w", err)
		}
	}
	twice := func(name string, set bool) error {
		if set {
			return fmt.Errorf("%w: %s given in --input and as a flag", dbstore.ErrInvalidInput, name)
		}
		return nil
	}
	if *f.sqlText != "" || len(*f.param) > 0 {
		return nil, fmt.Errorf("%w: db doc takes --filter/--params, not --sql/--param", dbstore.ErrInvalidInput)
	}
	for _, v := range []struct {
		name, flag string
		dst        *string
	}{{"collection", *f.collection, &in.Collection}, {"id", *f.docID, &in.ID}, {"index", *f.index, &in.Index},
		{"since", *f.since, &in.Since}, {"on_conflict", *f.onConflict, &in.OnConflict}} {
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
	}{{"doc", *f.doc, &in.Doc}, {"patch", *f.patch, &in.Patch}, {"filter", *f.filter, &in.Filter}, {"params", *f.params, &in.Params}} {
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
	}{{"limit", *f.limit, &in.Limit}, {"max_bytes", *f.maxBytes, &in.MaxBytes}} {
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
	if len(*f.fields) > 0 {
		if err := twice("fields", len(in.Fields) != 0); err != nil {
			return nil, err
		}
		in.Fields = *f.fields
	}
	in.IfAbsent = in.IfAbsent || *f.ifAbsent
	in.Unique = in.Unique || *f.unique
	// --out/--in/--replace mean the export directory and import mode here.
	if catOp == dbdoc.OpExport && *f.backup != "" {
		in.Out = *f.backup
	}
	if catOp == dbdoc.OpImport {
		if *f.source != "" {
			in.In = *f.source
		}
		in.Replace = in.Replace || *f.replace
	}
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	return json.Marshal(in)
}
