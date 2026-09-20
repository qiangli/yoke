// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package todo

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/spf13/cobra"
)

// Sprint 220, todo:4c6a5982 — classification is an OPEN vocabulary: `kind` is
// any one word (the common ones are suggestions in the help), labels are
// free words, and `todo kinds`/`todo labels` show what is in use instead of
// a whitelist saying what is allowed.
func TestClassificationIsOpenAndDiscoverable(t *testing.T) {
	t.Setenv("BASHY_TODO_DIR", t.TempDir())
	st, _ := UserStore("steward")
	sf := func() (*issue.Store, string, error) { return st, "test", nil }
	run := func(mk func(storeFunc) *cobra.Command, args ...string) string {
		t.Helper()
		var buf bytes.Buffer
		c := mk(sf)
		c.SetOut(&buf)
		c.SetErr(&buf)
		c.SetArgs(args)
		if err := c.Execute(); err != nil {
			t.Fatalf("%s %v: %v\n%s", c.Name(), args, err, buf.String())
		}
		return buf.String()
	}
	fail := func(mk func(storeFunc) *cobra.Command, args ...string) string {
		t.Helper()
		c := mk(sf)
		c.SetOut(&bytes.Buffer{})
		c.SetErr(&bytes.Buffer{})
		c.SetArgs(args)
		err := c.Execute()
		if err == nil {
			t.Fatalf("%s %v: expected an error", c.Name(), args)
		}
		return err.Error()
	}

	// A word nobody pre-registered is a kind. Labels split on commas and
	// lowercase; the help text names the common kinds without enforcing them.
	run(newAddCmd, "write the runbook", "--kind", "doc", "--label", "Docs,release")
	run(newAddCmd, "pipe on stdout", "--kind", "Bug", "--label", "windows")
	run(newAddCmd, "plain chore") // default kind: task
	if !strings.Contains(newAddCmd(sf).Flags().Lookup("kind").Usage, "bug|feature|enhancement|doc") {
		t.Fatal("the --kind help must list the common kinds")
	}

	// Only the SHAPE is refused, and the error says the rule.
	if msg := fail(newAddCmd, "x", "--kind", "bug fix"); !strings.Contains(msg, "one lowercase word") {
		t.Fatalf("shape error = %q", msg)
	}
	if msg := fail(newAddCmd, "x", "--label", "two words"); !strings.Contains(msg, "--label") {
		t.Fatalf("label shape error = %q", msg)
	}
	if n, _ := List(st, ""); len(n) != 3 {
		t.Fatalf("a refused add must write nothing: %d items", len(n))
	}

	// On disk it is the same frontmatter shape as before — nothing to migrate.
	items, _ := List(st, "")
	var doc *issue.Issue
	for _, it := range items {
		if it.Kind == "doc" {
			doc = it
		}
	}
	if doc == nil || strings.Join(doc.Labels, ",") != "docs,release" {
		t.Fatalf("doc item = %+v", doc)
	}
	b, _ := os.ReadFile(filepath.Join(st.Root, st.Sub, filepath.Base(mustFind(t, st, doc.ID))))
	if !strings.Contains(string(b), "kind: doc\n") || !strings.Contains(string(b), "labels:") {
		t.Fatalf("frontmatter:\n%s", b)
	}

	// Filters; the KIND column appears once something is not a task.
	out := run(newListCmd, "--kind", "BUG")
	if !strings.Contains(out, "pipe on stdout") || strings.Contains(out, "runbook") || !strings.Contains(out, "KIND") {
		t.Fatalf("list --kind:\n%s", out)
	}
	out = run(newListCmd, "--label", "release")
	if !strings.Contains(out, "runbook") || strings.Contains(out, "pipe on") {
		t.Fatalf("list --label:\n%s", out)
	}
	var env struct {
		Result struct {
			Items []listItem `json:"items"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(run(newListCmd, "--json", "--kind", "doc")), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Result.Items) != 1 || env.Result.Items[0].Kind != "doc" || len(env.Result.Items[0].Labels) != 2 {
		t.Fatalf("json rows = %+v", env.Result.Items)
	}

	// Edit: rekind, add and remove labels.
	run(newEditCmd, doc.ID, "--kind", "enhancement", "--label", "app", "--unlabel", "docs")
	got, _ := ResolveRef(st, doc.ID)
	if got.Kind != "enhancement" || strings.Join(got.Labels, ",") != "release,app" {
		t.Fatalf("after edit: kind=%q labels=%v", got.Kind, got.Labels)
	}

	// Discovery: most used first, then alphabetical; a typo would show as a
	// 1-count word beside the real one.
	kinds := run(func(sf storeFunc) *cobra.Command {
		return newWordsCmd(sf, "kinds", "", issue.KindsInUse)
	})
	if !strings.Contains(kinds, "1  bug") || !strings.Contains(kinds, "1  enhancement") || !strings.Contains(kinds, "1  task") {
		t.Fatalf("todo kinds:\n%s", kinds)
	}
	labels := run(func(sf storeFunc) *cobra.Command {
		return newWordsCmd(sf, "labels", "", issue.LabelsInUse)
	})
	lines := strings.Split(strings.TrimSpace(labels), "\n")
	if len(lines) != 4 || !strings.HasSuffix(lines[1], "app") || !strings.HasSuffix(lines[3], "windows") {
		t.Fatalf("todo labels (header + 3 words, alphabetical at equal count):\n%s", labels)
	}
}

// mustFind returns the on-disk path of an item (the store keeps it private).
func mustFind(t *testing.T, st *issue.Store, id string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(st.Root, st.Sub))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id) {
			return filepath.Join(st.Root, st.Sub, e.Name())
		}
	}
	t.Fatalf("no file for %s", id)
	return ""
}
