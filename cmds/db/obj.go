package dbcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dbobj"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/spf13/pflag"
)

// objVerb splits `db obj bucket create|add|drop|rm|list`, `db obj list|stat|get|put|rm|delete|transfer|export|import`
// (bucket-create and the like are the one-word spellings typed MCP uses) from the remaining operands.
func objVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db obj bucket create|drop|list|get|put|delete|rm|stat|list|transfer|export|import TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if sub == "bucket" && len(rest) > 0 {
		sub, rest = "bucket."+rest[0], rest[1:]
	} else if v, ok := strings.CutPrefix(sub, "bucket-"); ok {
		sub = "bucket." + v
	}
	switch sub {
	case "bucket.create", "bucket.add", "bucket.drop", "bucket.rm", "bucket.list",
		"list", "stat", "get", "put", "delete", "rm", "transfer", "export", "import":
		return "obj." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: db obj %s (want bucket create|drop|list, list, stat, get, put, rm, delete, transfer, export, import)", dbstore.ErrInvalidInput, operands[0])
}

// objFlags are the flags `db obj` reads besides the shared target and lifecycle ones.
var objFlags = map[string]bool{
	"engine": true, "mode": true, "path": true, "session": true, "name": true, "request-id": true, "attachment": true,
	"epoch": true, "json": true, "yes": true, "dry-run": true, "explain": true, "wait": true, "timeout": true, "input": true,
	"bucket": true, "key": true, "value": true, "content-type": true, "metadata": true, "in": true, "out": true,
	"prefix": true, "start": true, "limit": true, "max-bytes": true, "since": true,
	"if-revision": true, "if-absent": true, "on-conflict": true, "replace": true,
	"remote": true, "destination": true, "checksum": true, "delete": true,
}

// objValue parses text:UTF8 or b64:BASE64 or plain string into typed Value.
func objValue(flag, s string) (*dbobj.Value, error) {
	switch {
	case strings.HasPrefix(s, "text:"):
		t := strings.TrimPrefix(s, "text:")
		return &dbobj.Value{Text: &t}, nil
	case strings.HasPrefix(s, "b64:"):
		b := strings.TrimPrefix(s, "b64:")
		return &dbobj.Value{B64: &b}, nil
	default:
		return &dbobj.Value{Text: &s}, nil
	}
}

// objInput builds the one typed obj input from --input and the flags. A
// value given twice is an error, never a silent override.
func objInput(rc *tool.RunContext, f dbFlags) (json.RawMessage, error) {
	var foreign []string
	// Typed MCP passes every flag, so a flag at its default counts as unset.
	f.fs.Visit(func(fl *pflag.Flag) {
		if !objFlags[fl.Name] && fl.Value.String() != fl.DefValue {
			foreign = append(foreign, "--"+fl.Name)
		}
	})
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("%w: db obj does not take %s", dbstore.ErrInvalidInput, strings.Join(foreign, ", "))
	}
	var in dbobj.Input
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
		var decodeErr error
		in, decodeErr = dbobj.DecodeInput(bytes.TrimSpace(b))
		if decodeErr != nil {
			return nil, fmt.Errorf("--input: %w", decodeErr)
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
	}{
		{"bucket", *f.bucket, &in.Bucket},
		{"key", *f.key, &in.Key},
		{"content_type", *f.contentType, &in.ContentType},
		{"prefix", *f.prefix, &in.Prefix},
		{"start", *f.start, &in.Start},
		{"since", *f.since, &in.Since},
		{"if_revision", *f.ifRevision, &in.IfRevision},
		{"on_conflict", *f.onConflict, &in.OnConflict},
		{"remote", *f.remote, &in.Remote},
		{"destination", *f.destination, &in.Destination},
		{"checksum", *f.checksum, &in.Checksum},
	} {
		if v.flag != "" {
			if err := twice(v.name, *v.dst != ""); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	if *f.value != "" {
		if err := twice("value", in.Value != nil); err != nil {
			return nil, err
		}
		val, err := objValue("value", *f.value)
		if err != nil {
			return nil, err
		}
		in.Value = val
	}
	if *f.metadata != "" {
		if err := twice("metadata", len(in.Metadata) != 0); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(*f.metadata), &in.Metadata); err != nil {
			return nil, fmt.Errorf("%w: --metadata is not a JSON object: %v", dbstore.ErrInvalidInput, err)
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
	in.IfAbsent = in.IfAbsent || *f.ifAbsent
	in.Replace = in.Replace || *f.replace
	in.Delete = in.Delete || *f.deleteFlag

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
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	if in.Destination != "" && !strings.Contains(in.Destination, ":") {
		in.Destination = resolvePath(rc, in.Destination)
	}
	return json.Marshal(in)
}
