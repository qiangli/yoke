package dbcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/dbkv"
	"github.com/qiangli/yoke/pkg/dbstore"
)

// kvVerb splits `db kv bucket create|drop|list|get|put|delete|scan|cas|batch`
// (bucket-create and the like are the one-word spellings typed MCP uses).
func kvVerb(operands []string) (string, []string, error) {
	usage := fmt.Errorf("%w: db kv bucket create|drop|list|get|put|delete|scan|cas|batch TARGET", dbstore.ErrInvalidInput)
	if len(operands) == 0 {
		return "", nil, usage
	}
	sub, rest := operands[0], operands[1:]
	if sub == "bucket" && len(rest) > 0 {
		sub, rest = "bucket."+rest[0], rest[1:]
	} else if v, ok := strings.CutPrefix(sub, "bucket-"); ok {
		sub = "bucket." + v
	}
	switch sub {
	case "bucket.create", "bucket.drop", "bucket.list", "get", "put", "delete", "scan", "cas", "batch":
		return "kv." + sub, rest, nil
	}
	return "", nil, usage
}

// kvValue parses text:UTF8 or b64:BASE64 into the typed Value.
func kvValue(flag, s string) (*dbkv.Value, error) {
	switch {
	case strings.HasPrefix(s, "text:"):
		t := strings.TrimPrefix(s, "text:")
		return &dbkv.Value{Text: &t}, nil
	case strings.HasPrefix(s, "b64:"):
		b := strings.TrimPrefix(s, "b64:")
		return &dbkv.Value{B64: &b}, nil
	}
	return nil, fmt.Errorf("%w: --%s wants text:UTF8 or b64:BASE64", dbstore.ErrInvalidInput, flag)
}

// kvInput builds the typed kv input from --input and flags; a value given
// twice is an error, never a silent override.
func kvInput(rc *tool.RunContext, f dbFlags) (json.RawMessage, error) {
	var in dbkv.Input
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
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, fmt.Errorf("%w: --input: %v", dbstore.ErrInvalidInput, err)
		}
	}
	twice := func(name string, set bool) error {
		if set {
			return fmt.Errorf("%w: %s given in --input and as a flag", dbstore.ErrInvalidInput, name)
		}
		return nil
	}
	str := func(name, flag string, dst *string) error {
		if flag == "" {
			return nil
		}
		if err := twice(name, *dst != ""); err != nil {
			return err
		}
		*dst = flag
		return nil
	}
	val := func(name, flag string, dst **dbkv.Value) error {
		if flag == "" {
			return nil
		}
		if err := twice(name, *dst != nil); err != nil {
			return err
		}
		v, err := kvValue(name, flag)
		*dst = v
		return err
	}
	for _, e := range []error{
		str("bucket", *f.bucket, &in.Bucket), str("ttl", *f.ttl, &in.TTL), str("since", *f.since, &in.Since),
		val("key", *f.key, &in.Key), val("value", *f.value, &in.Value), val("expect", *f.expect, &in.Expect),
		val("start", *f.start, &in.Start), val("prefix", *f.prefix, &in.Prefix),
	} {
		if e != nil {
			return nil, e
		}
	}
	if *f.expectAbsent {
		if err := twice("expect_absent", in.ExpectAbsent); err != nil {
			return nil, err
		}
		in.ExpectAbsent = true
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
	return json.Marshal(in)
}
