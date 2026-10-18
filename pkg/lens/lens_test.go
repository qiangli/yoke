package lens

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

var update = flag.Bool("update", false, "rewrite testdata goldens")

func convert(t *testing.T, from, to string, data []byte) Result {
	t.Helper()
	res, err := Convert(context.Background(), Request{From: from, To: to, Data: data})
	if err != nil {
		t.Fatalf("%s -> %s: %v", from, to, err)
	}
	again, err := Convert(context.Background(), Request{From: from, To: to, Data: data})
	if err != nil || !bytes.Equal(res.Data, again.Data) {
		t.Fatalf("%s -> %s is not deterministic", from, to)
	}
	return res
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestLosslessRoundTrips pins A→B against a golden and B→A back to A's exact
// bytes for every lossless data row.
func TestLosslessRoundTrips(t *testing.T) {
	for _, c := range []struct{ src, from, to, out string }{
		{"doc.json", "json", "yaml", "doc.yaml"},
		{"config.toml", "toml", "json", "config.json"},
		{"config.toml", "toml", "yaml", "config.yaml"},
		{"rows.csv", "csv", "tsv", "rows.tsv"},
		{"rows.csv", "csv", "json", "rows.json"},
		{"records.jsonl", "jsonl", "json", "records.json"},
	} {
		t.Run(c.src+"->"+c.to, func(t *testing.T) {
			a := read(t, c.src)
			fwd := convert(t, c.from, c.to, a)
			if len(fwd.Losses) > 0 || fwd.Row.Fidelity != Lossless {
				t.Fatalf("forward lossy: %v", fwd.Losses)
			}
			golden(t, c.out, fwd.Data)
			back := convert(t, c.to, c.from, fwd.Data)
			if len(back.Losses) > 0 {
				t.Fatalf("back lossy: %v", back.Losses)
			}
			if !bytes.Equal(back.Data, a) {
				t.Fatalf("round trip differs\n--- got\n%s\n--- want\n%s", back.Data, a)
			}
		})
	}
	// json → toml → json identity needs toml's canonical key order (tables
	// last), which config.json has by construction.
	cfg := read(t, "config.json")
	tm := convert(t, "json", "toml", cfg)
	if len(tm.Losses) > 0 || !bytes.Equal(tm.Data, read(t, "config.toml")) {
		t.Fatalf("json -> toml: losses=%v\n%s", tm.Losses, tm.Data)
	}
	if back := convert(t, "json", "jsonl", read(t, "records.json")); !bytes.Equal(back.Data, read(t, "records.jsonl")) {
		t.Fatalf("json -> jsonl:\n%s", back.Data)
	}
	if back := convert(t, "tsv", "csv", read(t, "rows.tsv")); !bytes.Equal(back.Data, read(t, "rows.csv")) {
		t.Fatalf("tsv -> csv:\n%s", back.Data)
	}
}

func TestRuntimeLosses(t *testing.T) {
	for _, c := range []struct {
		from, to, in, want string
	}{
		{"json", "toml", `{"a": null, "b": 1}`, "no null"},
		{"json", "toml", `{"t": {"x": 1}, "late": 2}`, "key order changed"},
		{"yaml", "json", "a: 1 # note\n", "comments dropped"},
		{"yaml", "json", "base: &b {x: 1}\nuse: *b\n", "anchors/aliases"},
		{"yaml", "json", "a: 1\n---\nb: 2\n", "wrapped in one array"},
		{"yaml", "json", "x: .inf\n", "no JSON number form"},
		{"toml", "json", "d = 2026-10-17\n", "datetime"},
		{"json", "csv", `[{"a": 1, "b": null}]`, "null"},
		{"json", "jsonl", `{"a": 1}`, "single line"},
		{"csv", "json", "a,a\n1,2\n", "duplicate column"},
		{"csv", "md", "a\n\"x\ny\"\n", "<br>"},
		{"json", "yaml", `{"a": 1, "a": 2}`, "duplicate key"},
	} {
		res := convert(t, c.from, c.to, []byte(c.in))
		if !res.Lossy() || !strings.Contains(strings.Join(res.Losses, ";"), c.want) {
			t.Errorf("%s -> %s %q: losses %v, want %q", c.from, c.to, c.in, res.Losses, c.want)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	for _, c := range []struct{ from, to, in string }{
		{"json", "yaml", `{"a": `},
		{"json", "yaml", `{} {}`},
		{"yaml", "json", "a: [\n"},
		{"toml", "json", "a = \n"},
		{"json", "toml", `[1, 2]`},
		{"json", "csv", `{"a": 1}`},
		{"csv", "json", "a,\"b\n"},
		{"pdf", "txt", "%PDF-1.4 truncated"},
		{"docx", "md", "not a zip"},
		{"zip", "listing", "not a zip"},
	} {
		_, err := Convert(context.Background(), Request{From: c.from, To: c.to, Data: []byte(c.in)})
		var le *Error
		if !errors.As(err, &le) || le.Code != ExitInvalid {
			t.Errorf("%s -> %s %q: err %v, want exit %d", c.from, c.to, c.in, err, ExitInvalid)
		}
	}
}

func TestTables(t *testing.T) {
	golden(t, "rows.md", convert(t, "csv", "md", read(t, "rows.csv")).Data)
	golden(t, "rows-tsv.md", convert(t, "tsv", "md", read(t, "rows.tsv")).Data)
	golden(t, "doc-to.csv", convert(t, "json", "csv", read(t, "rows.json")).Data)
}

func TestHTML(t *testing.T) {
	html := read(t, "page.html")
	md := convert(t, "html", "md", html)
	golden(t, "page.md", md.Data)
	shared, err := HTMLToMarkdown(string(html))
	if err != nil || string(md.Data) != strings.TrimRight(shared, "\n")+"\n" {
		t.Fatalf("lens html->md diverges from HTMLToMarkdown (fetch --md): %v", err)
	}
	golden(t, "page.txt", convert(t, "html", "txt", html).Data)
}

func TestDocumentFallbacks(t *testing.T) {
	for _, c := range []struct {
		from string
		data []byte
	}{
		{"pdf", pdfFixture()},
		{"docx", docxFixture(t)},
		{"xlsx", xlsxFixture(t)},
	} {
		for _, to := range []string{"md", "txt"} {
			res := convert(t, c.from, to, c.data)
			if res.Row.Via() != "go" || res.Row.Fidelity != Lossy {
				t.Fatalf("%s -> %s via %s (%s)", c.from, to, res.Row.Via(), res.Row.Fidelity)
			}
			golden(t, c.from+"."+to, res.Data)
		}
	}
}

func TestListing(t *testing.T) {
	zipData := zipOf(t, [][2]string{{"docs/", ""}, {"docs/a.md", "# a"}, {"b.bin", "0123456789"}})
	golden(t, "zip.listing", convert(t, "zip", "listing", zipData).Data)
	golden(t, "zip.md", convert(t, "zip", "md", zipData).Data)

	plain := convert(t, "tar", "listing", tarOf(t, false, tar.FormatUSTAR)).Data
	golden(t, "tar.listing", plain)
	for name, data := range map[string][]byte{
		"tar.gz": tarOf(t, true, tar.FormatUSTAR),
		"pax":    tarOf(t, false, tar.FormatPAX),
	} {
		if got := convert(t, "tar", "listing", data).Data; !bytes.Equal(got, plain) {
			t.Errorf("%s listing differs from plain tar:\n%s", name, got)
		}
	}
}

func TestDetect(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want string
	}{
		{"a.json", nil, "json"},
		{"a.YML", nil, "yaml"},
		{"a.ndjson", nil, "jsonl"},
		{"a.tab", nil, "tsv"},
		{"a.tar.gz", nil, "tar"},
		{"a.tgz", nil, "tar"},
		{"a.pax", nil, "tar"},
		{"", []byte("%PDF-1.7\n"), "pdf"},
		{"", docxFixture(t), "docx"},
		{"", xlsxFixture(t), "xlsx"},
		{"", zipOf(t, [][2]string{{"x", "y"}}), "zip"},
		{"", tarOf(t, false, tar.FormatUSTAR), "tar"},
		{"", tarOf(t, true, tar.FormatUSTAR), "tar"},
		{"blob", []byte("  {\"a\": 1}"), "json"},
		{"", []byte("{\"a\":1}\n{\"a\":2}\n"), "jsonl"},
		{"", []byte("<!DOCTYPE html><html></html>"), "html"},
		{"", []byte("a: 1\n"), ""},
		{"", []byte("a,b\n1,2\n"), ""},
	} {
		if got := Detect(c.name, c.data); got != c.want {
			t.Errorf("Detect(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	if _, err := Convert(context.Background(), Request{To: "json", Data: []byte("a: 1\n")}); exitCode(err) != ExitUsage {
		t.Errorf("undetectable input: %v", err)
	}
}

func exitCode(err error) int {
	var le *Error
	if errors.As(err, &le) {
		return le.Code
	}
	return -1
}

func TestRouting(t *testing.T) {
	if _, err := Route("pdf", "yaml"); exitCode(err) != ExitNoRoute {
		t.Errorf("pdf->yaml: %v", err)
	}
	if _, err := Route("pptx", "md"); exitCode(err) != ExitRecipeMissing {
		t.Errorf("pptx->md without a registered recipe: %v", err)
	}
	if r, err := Route("pdf", "md"); err != nil || r.Via() != "go" {
		t.Errorf("pdf->md fallback: %v %v", r.Via(), err)
	}
	seen := map[string]bool{}
	for _, r := range Rows() {
		key := r.From + ">" + r.To + ">" + r.Via()
		if seen[key] {
			t.Errorf("duplicate row %s", key)
		}
		seen[key] = true
		if Canonical(r.From) != r.From || Canonical(r.To) != r.To {
			t.Errorf("row %s uses a non-canonical format", key)
		}
	}
}

func TestRecipePreferredWhenRegistered(t *testing.T) {
	var gotArgs []string
	fake := &tool.Tool{Name: "markitdown", Run: func(rc *tool.RunContext, args []string) int {
		gotArgs = args
		data, err := os.ReadFile(args[0])
		if err != nil {
			return 1
		}
		fmt.Fprintf(rc.Out, "# via recipe (%d bytes)\n", len(data))
		return 0
	}}
	defer func(prev func(string) *tool.Tool) { LookupCommand = prev }(LookupCommand)
	LookupCommand = func(name string) *tool.Tool {
		if name == "markitdown" {
			return fake
		}
		return nil
	}
	pdf := pdfFixture()
	res, err := Convert(context.Background(), Request{From: "pdf", To: "md", Name: "report.pdf", Data: pdf})
	if err != nil {
		t.Fatal(err)
	}
	if res.Row.Via() != "recipe:markitdown" || string(res.Data) != fmt.Sprintf("# via recipe (%d bytes)\n", len(pdf)) {
		t.Fatalf("via %s: %q", res.Row.Via(), res.Data)
	}
	if len(gotArgs) != 1 || filepath.Base(gotArgs[0]) != "report.pdf" {
		t.Fatalf("recipe args %v", gotArgs)
	}
	// txt has no recipe row: still the Go fallback.
	if res, err := Convert(context.Background(), Request{From: "pdf", To: "txt", Data: pdf}); err != nil || res.Row.Via() != "go" {
		t.Fatalf("pdf->txt: %v %v", res.Row.Via(), err)
	}
	// A failing recipe is invalid input, not a silent fallback.
	fake.Run = func(rc *tool.RunContext, args []string) int { fmt.Fprintln(rc.Err, "boom"); return 3 }
	if _, err := Convert(context.Background(), Request{From: "pptx", To: "md", Data: []byte("x")}); exitCode(err) != ExitInvalid || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failing recipe: %v", err)
	}
}
