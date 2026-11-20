package seedfit

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// LineageChain is one model line, oldest first: vendor<TAB>line<TAB>m1>m2>…
type LineageChain struct {
	Vendor, Line string
	Models       []string
}

// ReadLineage parses a lineage file ('#' comments and blank lines skipped).
func ReadLineage(r io.Reader) ([]LineageChain, error) {
	var out []LineageChain
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		f := strings.Split(t, "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("lineage line %d: want vendor<TAB>line<TAB>old>…>new", n)
		}
		var ms []string
		for _, m := range strings.Split(f[2], ">") {
			if m = strings.TrimSpace(m); m != "" {
				ms = append(ms, m)
			}
		}
		if len(ms) < 2 {
			return nil, fmt.Errorf("lineage line %d: a chain needs two models", n)
		}
		out = append(out, LineageChain{Vendor: f[0], Line: f[1], Models: ms})
	}
	return out, sc.Err()
}

// Inheritance records one row a model inherited from its predecessor.
type Inheritance struct {
	Model, From, Benchmark, Variant string
}

// ApplyLineage gives a model, for every (benchmark, variant) it has no row
// for, the row of its newest predecessor in the same line that has one. The
// value is carried as-is (no uplift), keeps its source type so the source
// offset still applies, and is flagged in Row.Inherited. Real rows always win.
func ApplyLineage(rows []Row, chains []LineageChain) ([]Row, []Inheritance) {
	type key struct{ m, b, v string }
	have := map[key]Row{}
	for _, r := range rows {
		k := key{r.Model, r.Benchmark, r.Variant}
		if _, ok := have[k]; !ok {
			have[k] = r
		}
	}
	items := map[[2]string]bool{}
	var order [][2]string
	for _, r := range rows {
		it := [2]string{r.Benchmark, r.Variant}
		if !items[it] {
			items[it] = true
			order = append(order, it)
		}
	}
	out := append([]Row(nil), rows...)
	var log []Inheritance
	for _, c := range chains {
		for i := len(c.Models) - 1; i >= 1; i-- {
			newer := c.Models[i]
			for _, it := range order {
				if _, ok := have[key{newer, it[0], it[1]}]; ok {
					continue
				}
				for j := i - 1; j >= 0; j-- {
					src, ok := have[key{c.Models[j], it[0], it[1]}]
					if !ok {
						continue
					}
					src.Model = newer
					src.Inherited = c.Models[j]
					out = append(out, src)
					log = append(log, Inheritance{newer, c.Models[j], it[0], it[1]})
					break
				}
			}
		}
	}
	return out, log
}
