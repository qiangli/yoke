// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fence builds a fenced code block. Double-quoted strings may contain
// backticks (only raw string literals may not), so "```" is legal here.
func block(lang, body string) string {
	return "```" + lang + "\n" + body + "\n```\n"
}

func TestParseBasic(t *testing.T) {
	md := "## Tasks\n\n" +
		"### build\n" +
		"Compile the app.\n" +
		"Requires: clean, deps\n" +
		"Sources: src/\n" +
		"Generates: bin/app\n" +
		block("bash", "go build -o bin/app .")

	doc, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.Tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(doc.Tasks))
	}
	got := doc.Tasks[0]
	if got.Name != "build" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Desc != "Compile the app." {
		t.Errorf("desc = %q", got.Desc)
	}
	if got.Lang != "bash" {
		t.Errorf("lang = %q", got.Lang)
	}
	if !reflect.DeepEqual(got.Requires, []string{"clean", "deps"}) {
		t.Errorf("requires = %v", got.Requires)
	}
	if !reflect.DeepEqual(got.Sources, []string{"src/"}) {
		t.Errorf("sources = %v", got.Sources)
	}
	if !reflect.DeepEqual(got.Generates, []string{"bin/app"}) {
		t.Errorf("generates = %v", got.Generates)
	}
	if strings.TrimSpace(got.Body) != "go build -o bin/app ." {
		t.Errorf("body = %q", got.Body)
	}
}

func TestParseTopLevelTasks(t *testing.T) {
	// No "## Tasks" section => top-level "## name" headings are targets.
	md := "# Title (ignored)\n\n" +
		"## a\n" + block("", "echo a") +
		"## b\nRequires: a\n" + block("", "echo b")
	doc, err := Parse(strings.NewReader(md), "x.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(doc.Order, []string{"a", "b"}) {
		t.Fatalf("order = %v", doc.Order)
	}
	if got := doc.Tasks[0].Lang; got != "" {
		t.Errorf("empty fence should yield lang \"\", got %q", got)
	}
}

func TestParseUnknownKeyAndContractMeta(t *testing.T) {
	// Ensure:/Effects: are P2 metadata (captured). An unrecognized "Foo:" key
	// is prose, not metadata (so a colon in prose never trips the parser).
	md := "## Tasks\n\n### pkg\n" +
		"Bundle it.\n" +
		"Effects: write\n" +
		"Ensure: file-exists path=dist/out\n" +
		"Note: this line is prose\n" +
		block("bash", "tar -cf dist/out .")
	doc, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := doc.Tasks[0]
	if !reflect.DeepEqual(got.Effects, []string{"write"}) {
		t.Errorf("effects = %v", got.Effects)
	}
	if !reflect.DeepEqual(got.Ensure, []string{"file-exists path=dist/out"}) {
		t.Errorf("ensure = %v", got.Ensure)
	}
	if !strings.Contains(got.Desc, "Note: this line is prose") {
		t.Errorf("unknown key should be prose; desc = %q", got.Desc)
	}
}

func TestParseTimeoutRetries(t *testing.T) {
	md := "## Tasks\n\n### flaky\n" +
		"Timeout: 90s\n" +
		"Retries: 3 backoff=2s\n" +
		block("bash", "echo go")
	doc, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := doc.Tasks[0]
	if got.Timeout != 90*time.Second {
		t.Errorf("timeout = %s", got.Timeout)
	}
	if got.Retries != 3 {
		t.Errorf("retries = %d", got.Retries)
	}
	if got.Backoff != 2*time.Second {
		t.Errorf("backoff = %s", got.Backoff)
	}
}

func TestParseExitCodes(t *testing.T) {
	md := "## Tasks\n\n### classify\nExitCodes: 0=ok 75=skip 2=retry 9=fail\nHost: host-a\n" +
		block("bash", "exit 75")
	doc, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := doc.Tasks[0].ExitCodes
	want := map[int]string{0: "ok", 75: "skip", 2: "retry", 9: "fail"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exit codes = %#v, want %#v", got, want)
	}
	if doc.Tasks[0].Host != "host-a" {
		t.Errorf("host = %q", doc.Tasks[0].Host)
	}
}

func TestParseDistributionAndClusterLane(t *testing.T) {
	md := "## Tasks\n\n### pretrain\n" +
		"Distribution: shardable\n" +
		"Lane: cluster\n" +
		block("bash", "python train.py")
	doc, err := Parse(strings.NewReader(md), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := doc.Tasks[0]
	if got.Distribution != DistributionShardable {
		t.Errorf("distribution = %q, want %q", got.Distribution, DistributionShardable)
	}
	if got.Venue != VenueCluster {
		t.Errorf("venue = %q, want %q", got.Venue, VenueCluster)
	}
	if strings.Contains(got.Desc, "Distribution:") || strings.Contains(got.Desc, "Lane:") {
		t.Errorf("recognized metadata leaked into description: %q", got.Desc)
	}
}

func TestParseFrontmatter(t *testing.T) {
	md := "---\nname: demo\ndescription: A demo pipeline\n---\n\n" +
		"## Tasks\n\n### t\n" + block("", "echo t")
	doc, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Name != "demo" || doc.Desc != "A demo pipeline" {
		t.Errorf("frontmatter = %q / %q", doc.Name, doc.Desc)
	}
	if len(doc.Tasks) != 1 || doc.Tasks[0].Name != "t" {
		t.Errorf("tasks = %+v", doc.Tasks)
	}
}

func TestParseFileIncludes(t *testing.T) {
	dir := t.TempDir()
	// lib.md defines `compile`; DAG.md includes it and a `build` that requires it.
	if err := os.WriteFile(filepath.Join(dir, "lib.md"),
		[]byte("## Tasks\n\n### compile\n"+block("bash", "echo compile")), 0o644); err != nil {
		t.Fatal(err)
	}
	main := "---\ninclude: lib.md\n---\n\n## Tasks\n\n### build\nRequires: compile\n" +
		block("bash", "echo build")
	if err := os.WriteFile(filepath.Join(dir, "DAG.md"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := ParseFile(filepath.Join(dir, "DAG.md"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if _, ok := doc.Lookup("compile"); !ok {
		t.Fatalf("included target 'compile' not merged; order=%v", doc.Order)
	}
	// The graph resolves a cross-file dependency edge.
	if _, err := BuildGraph(doc); err != nil {
		t.Errorf("BuildGraph across include: %v", err)
	}
}

func TestParseBodyCommentNotHeading(t *testing.T) {
	// Regression: a `#`/`##` shell comment INSIDE a fenced body must not be read
	// as a markdown heading (it used to flush the target early and drop the rest).
	md := "## Tasks\n\n" +
		"### a\n```bash\n# a shell comment\n## still a comment, not a heading\necho a\n```\n" +
		"### b\nRequires: a\n" + block("bash", "echo b")
	d, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !reflect.DeepEqual(d.Order, []string{"a", "b"}) {
		t.Fatalf("want [a b], got %v (a body comment broke target boundaries)", d.Order)
	}
	ta, _ := d.Lookup("a")
	if !strings.Contains(ta.Body, "# a shell comment") {
		t.Errorf("comment lost from body: %q", ta.Body)
	}
}

func TestParseDuplicateTarget(t *testing.T) {
	md := "## Tasks\n\n### a\n" + block("", "echo 1") + "### a\n" + block("", "echo 2")
	if _, err := Parse(strings.NewReader(md), "DAG.md"); err == nil {
		t.Fatal("want duplicate-target error, got nil")
	} else if ExitCodeOf(err) != 2 {
		t.Errorf("want exit 2, got %d", ExitCodeOf(err))
	}
}
