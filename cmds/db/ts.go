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
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbts"
	"github.com/spf13/pflag"
)

// tsVerb splits `db ts ingest|range|agg|retain|export|import` and
// `db ts remote query|ingest` (remote-query and the like are the one-word
// spellings typed MCP uses) from the remaining operands.
func tsVerb(operands []string) (string, []string, error) {
	if len(operands) == 0 {
		return "", nil, fmt.Errorf("%w: db ts ingest|range|agg|retain|export|import|remote query|ingest TARGET", dbstore.ErrInvalidInput)
	}
	sub, rest := operands[0], operands[1:]
	if sub == "remote" && len(rest) > 0 {
		sub, rest = "remote."+rest[0], rest[1:]
	} else if v, ok := strings.CutPrefix(sub, "remote-"); ok {
		sub = "remote." + v
	}
	switch sub {
	case "ingest", "range", "agg", "retain", "export", "import", "remote.query", "remote.ingest":
		return "ts." + sub, rest, nil
	}
	return "", nil, fmt.Errorf("%w: unknown ts verb %s", dbstore.ErrInvalidInput, sub)
}

// tsFlags are the flags `db ts` reads besides the shared target and
// lifecycle ones; any other data flag (--sql, --doc, --collection, …) is
// invalid_input rather than silently ignored. --bucket and --endpoint are
// shared flags reused with ts meaning (agg time bucket, remote endpoint),
// the same way --id and --input already serve every category.
var tsFlags = map[string]bool{
	"engine": true, "mode": true, "path": true, "session": true, "name": true, "request-id": true, "attachment": true,
	"epoch": true, "json": true, "yes": true, "dry-run": true, "explain": true, "wait": true, "timeout": true, "input": true,
	"series": true, "labels": true, "points": true, "bucket": true, "funcs": true, "retention": true,
	"expr": true, "step": true, "endpoint": true,
	"limit": true, "max-bytes": true, "since": true, "on-conflict": true, "out": true, "in": true, "replace": true,
}

// tsInput builds the one typed ts input from --input and the flags. A
// value given twice is an error, never a silent override. Timestamp bounds
// (start/end) and the remote series_list are typed JSON values, so they
// travel in --input only; every scalar has a flag.
func tsInput(rc *tool.RunContext, f dbFlags) (json.RawMessage, error) {
	var foreign []string
	// Typed MCP passes every flag, so a flag at its default counts as unset.
	f.fs.Visit(func(fl *pflag.Flag) {
		if !tsFlags[fl.Name] && fl.Value.String() != fl.DefValue {
			foreign = append(foreign, "--"+fl.Name)
		}
	})
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("%w: db ts does not take %s", dbstore.ErrInvalidInput, strings.Join(foreign, ", "))
	}
	var in dbts.Input
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
		if in, err = dbts.DecodeInput(bytes.TrimSpace(b)); err != nil {
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
	}{{"series", *f.tsSeries, &in.Series}, {"bucket", *f.bucket, &in.Bucket},
		{"retention", *f.tsRetention, &in.Retention}, {"expr", *f.tsExpr, &in.Expr},
		{"step", *f.tsStep, &in.Step}, {"endpoint", *f.endpoint, &in.Endpoint},
		{"since", *f.since, &in.Since}, {"on_conflict", *f.onConflict, &in.OnConflict}} {
		if v.flag != "" {
			if err := twice(v.name, *v.dst != ""); err != nil {
				return nil, err
			}
			*v.dst = v.flag
		}
	}
	if *f.tsLabels != "" {
		if err := twice("labels", len(in.Labels) != 0); err != nil {
			return nil, err
		}
		var labels map[string]string
		if err := json.Unmarshal([]byte(*f.tsLabels), &labels); err != nil || labels == nil {
			return nil, fmt.Errorf("%w: --labels wants a JSON object of strings", dbstore.ErrInvalidInput)
		}
		in.Labels = labels
	}
	if *f.tsPoints != "" {
		if err := twice("points", len(in.Points) != 0); err != nil {
			return nil, err
		}
		var points []dbts.Point
		if err := json.Unmarshal([]byte(*f.tsPoints), &points); err != nil || points == nil {
			return nil, fmt.Errorf("%w: --points wants a JSON array of {ts, value} samples", dbstore.ErrInvalidInput)
		}
		in.Points = points
	}
	if *f.tsFuncs != "" {
		if err := twice("funcs", len(in.Funcs) != 0); err != nil {
			return nil, err
		}
		for _, fn := range strings.Split(*f.tsFuncs, ",") {
			if fn = strings.TrimSpace(fn); fn != "" {
				in.Funcs = append(in.Funcs, fn)
			}
		}
		if len(in.Funcs) == 0 {
			return nil, fmt.Errorf("%w: --funcs wants a comma-separated subset of count,min,max,sum,avg", dbstore.ErrInvalidInput)
		}
	}
	if *f.limit != 0 {
		if err := twice("limit", in.Limit != 0); err != nil {
			return nil, err
		}
		in.Limit = *f.limit
	}
	if *f.maxBytes != 0 {
		if err := twice("max_bytes", in.MaxBytes != 0); err != nil {
			return nil, err
		}
		in.MaxBytes = *f.maxBytes
	}
	if *f.replace && in.Replace {
		return nil, fmt.Errorf("%w: duplicate replace", dbstore.ErrInvalidInput)
	}
	in.Replace = in.Replace || *f.replace
	if *f.backup != "" {
		if err := twice("out", in.Out != ""); err != nil {
			return nil, err
		}
		in.Out = *f.backup
	}
	if *f.source != "" {
		if err := twice("in", in.In != ""); err != nil {
			return nil, err
		}
		in.In = *f.source
	}
	if in.Out != "" && in.Out != "-" {
		in.Out = resolvePath(rc, in.Out)
	}
	if in.In != "" && in.In != "-" {
		in.In = resolvePath(rc, in.In)
	}
	return json.Marshal(in)
}
