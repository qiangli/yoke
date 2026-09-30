package seedfit

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// matrixHeader is the column set of the public benchmark matrix. Columns are
// located by name, so extra or reordered columns are tolerated; a missing
// required column is an error.
var matrixHeader = []string{
	"vendor", "vendor_cli", "fleet_model", "in_bashy_seeds", "vendor_model_id",
	"capability_area", "benchmark", "variant_or_subscore", "metric", "score",
	"agent_tool_or_harness", "settings", "date", "source_type", "source_url",
	"notes", "collector",
}

var requiredColumns = []string{
	"vendor", "vendor_cli", "fleet_model", "capability_area", "benchmark",
	"variant_or_subscore", "metric", "score", "agent_tool_or_harness", "source_type",
}

// Row is one matrix row, reduced to the columns the fit reads.
type Row struct {
	Vendor, VendorCLI, Model string
	Area, Benchmark, Variant string
	Metric, Score            string
	Harness, SourceType      string
	SourceURL, Date          string
	Line                     int // 1-based line in the input file
}

// ReadMatrix parses the tab-separated matrix. The first line is the header.
func ReadMatrix(r io.Reader) ([]Row, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("matrix: empty input")
	}
	col := map[string]int{}
	for i, h := range strings.Split(strings.TrimRight(sc.Text(), "\r"), "\t") {
		col[strings.TrimSpace(h)] = i
	}
	for _, h := range requiredColumns {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("matrix: missing column %q", h)
		}
	}
	var rows []Row
	line := 1
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(text) == "" {
			continue
		}
		f := strings.Split(text, "\t")
		get := func(name string) string {
			if i := col[name]; i < len(f) {
				return strings.TrimSpace(f[i])
			}
			return ""
		}
		rows = append(rows, Row{
			Vendor: get("vendor"), VendorCLI: get("vendor_cli"), Model: get("fleet_model"),
			Area: get("capability_area"), Benchmark: get("benchmark"), Variant: get("variant_or_subscore"),
			Metric: get("metric"), Score: get("score"),
			Harness: get("agent_tool_or_harness"), SourceType: get("source_type"),
			SourceURL: get("source_url"), Date: get("date"),
			Line: line,
		})
	}
	return rows, sc.Err()
}

// OrderChain is one declared within-generation ordering: Models[0] is
// declared at least as able as Models[1], and so on.
type OrderChain struct {
	Vendor, Generation string
	Models             []string
	Line               int
}

// ReadOrder parses an order file: one chain per line,
// "vendor<TAB>generation<TAB>model1>model2>...". Blank lines and lines
// starting with '#' are ignored.
func ReadOrder(r io.Reader) ([]OrderChain, error) {
	sc := bufio.NewScanner(r)
	var out []OrderChain
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		f := strings.Split(text, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("order line %d: want vendor<TAB>generation<TAB>m1>m2>..., got %d fields", line, len(f))
		}
		var models []string
		for _, m := range strings.Split(f[2], ">") {
			if m = strings.TrimSpace(m); m != "" {
				models = append(models, m)
			}
		}
		if len(models) == 0 {
			return nil, fmt.Errorf("order line %d: no models", line)
		}
		out = append(out, OrderChain{
			Vendor: strings.TrimSpace(f[0]), Generation: strings.TrimSpace(f[1]),
			Models: models, Line: line,
		})
	}
	return out, sc.Err()
}
