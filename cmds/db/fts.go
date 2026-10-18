package dbcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dbfts"
	"github.com/qiangli/yoke/pkg/dbstore"
)

func ftsVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db fts VERB TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if sub == "mapping" && len(rest) > 0 {
		sub, rest = "mapping."+rest[0], rest[1:]
	}
	switch sub {
	case "search", "get", "put", "delete", "batch", "check", "mapping.show", "rebuild":
		return "fts." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: unknown fts verb %s", dbstore.ErrInvalidInput, sub)
}

func ftsInput(rc *tool.RunContext, f dbFlags, op string) (json.RawMessage, error) {
	var in dbfts.Input
	if *f.input != "" {
		var r io.Reader = rc.In
		if *f.input != "-" {
			file, err := os.Open(resolvePath(rc, *f.input))
			if err != nil {
				return nil, err
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
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("%w: fts input: %v", dbstore.ErrInvalidInput, err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("%w: trailing fts input", dbstore.ErrInvalidInput)
		}
	}
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
	if *f.since != "" {
		if in.Since != "" {
			return nil, fmt.Errorf("%w: duplicate since", dbstore.ErrInvalidInput)
		}
		in.Since = *f.since
	}
	if *f.onConflict != "" {
		if in.OnConflict != "" {
			return nil, fmt.Errorf("%w: duplicate on_conflict", dbstore.ErrInvalidInput)
		}
		in.OnConflict = *f.onConflict
	}
	if op == "export" {
		in.Out = *f.backup
	}
	if op == "import" {
		in.In = *f.source
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
