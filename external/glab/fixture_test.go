package glab

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/adapt"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Offline fixture: a fake `glab` shell script on PATH emulating the
// provider's recorded JSON contract (list two pages, create/delete a note
// round-trip through a state file, a canary-bearing failure, a blocking
// wait). No network, no account, no real data.
const fixtureScript = `#!/bin/sh
# recorded glab-shaped contract: $FIXTURE_STATE holds created ids
case "$1" in
  list-page1) printf '[{"id":1},{"id":2}]' ;;
  list-page2) printf '[{"id":3}]' ;;
  create) printf '{"id":7,"title":"fixture"}' > "$FIXTURE_STATE"; cat "$FIXTURE_STATE" ;;
  delete)
    if [ -f "$FIXTURE_STATE" ]; then rm -f "$FIXTURE_STATE"; printf '{"deleted":true}';
    else printf '{"deleted":false}'; exit 1; fi ;;
  denied) printf '403 forbidden for CANARY-GLAB-FIXTURE-TOKEN-zz99' >&2; exit 1 ;;
  wait) exec sleep 30 ;;
  *) printf 'unknown fixture command' >&2; exit 2 ;;
esac
`

func fixtureBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell script")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "glab")
	if err := os.WriteFile(p, []byte(fixtureScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURE_STATE", filepath.Join(dir, "state.json"))
	return p
}

func runFixture(ctx context.Context, bin string, args ...string) (string, string, error) {
	cmd := binmgr.Command(ctx, bin, args...)
	var out, diag bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diag
	err := cmd.Run()
	return out.String(), diag.String(), err
}

// Read: two recorded pages decode as JSON and paginate through page.go.
func TestFixtureReadAndPagination(t *testing.T) {
	bin := fixtureBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var recs []struct {
		ID int `json:"id"`
	}
	for _, sub := range []string{"list-page1", "list-page2"} {
		out, diag, err := runFixture(ctx, bin, sub)
		if err != nil {
			t.Fatalf("%s: %v %s", sub, err, diag)
		}
		var page []struct {
			ID int `json:"id"`
		}
		if uerr := json.Unmarshal([]byte(out), &page); uerr != nil {
			t.Fatalf("%s: %v", sub, uerr)
		}
		recs = append(recs, page...)
	}
	if len(recs) != 3 || recs[0].ID != 1 || recs[2].ID != 3 {
		t.Fatalf("records = %+v, want ids 1,2,3", recs)
	}
	raw := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		raw = append(raw, json.RawMessage([]byte(`{"id":`+strconv.Itoa(r.ID)+`}`)))
	}
	p, err := Paginate(raw, 0, 2, Binding{Target: "issues:list", Query: "state=opened"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Complete || len(p.Items) != 2 || p.Total != 3 {
		t.Fatalf("page = %+v, want partial 2/3", p)
	}
	t.Log("read: fixture pages decode as JSON and paginate (2+1 of 3)")
}

// Reversible write: create then delete round-trips through fixture state,
// leaving no residue.
func TestFixtureReversibleWrite(t *testing.T) {
	bin := fixtureBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	state := os.Getenv("FIXTURE_STATE")
	out, diag, err := runFixture(ctx, bin, "create")
	if err != nil {
		t.Fatalf("create: %v %s", err, diag)
	}
	if !strings.Contains(out, `"id":7`) {
		t.Fatalf("create output = %q", out)
	}
	out, diag, err = runFixture(ctx, bin, "delete")
	if err != nil {
		t.Fatalf("delete: %v %s", err, diag)
	}
	if !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("delete output = %q", out)
	}
	if _, serr := os.Stat(state); !os.IsNotExist(serr) {
		t.Fatal("fixture state not cleaned up")
	}
	t.Log("reversible write: create/delete round-trip with cleanup; no live support claimed (Sprint 465)")
}

// Redaction: the fixture failure carries a canary; diagnostics must not.
func TestFixtureFailureRedaction(t *testing.T) {
	const canary = "CANARY-GLAB-FIXTURE-TOKEN-zz99"
	bin := fixtureBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, diag, err := runFixture(ctx, bin, "denied")
	if err == nil {
		t.Fatal("denied fixture reported success")
	}
	if got := RedactText(diag, canary); strings.Contains(got, canary) {
		t.Fatalf("failure diagnostics leak canary: %q", diag)
	}
	if !strings.Contains(diag, "403") {
		t.Fatalf("diagnostics lost safe context: %q", diag)
	}
	t.Log("redaction: provider-failure canary masked, safe context kept")
}

// Cancellation: the fixture holds /wait open; cancel must kill and reap.
func TestFixtureCancellation(t *testing.T) {
	bin := fixtureBinary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := runFixture(ctx, bin, "wait")
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reap fixture")
	}
	t.Log("cancellation: child returned failure and was reaped")
}

// glab has no dry-run: the shipped adapt row refuses before execution.
func TestFixtureAdaptRefusesDryRun(t *testing.T) {
	rows, errs := fleet.New(fleet.WithoutCloudOverlay()).Adapters()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, err := adapt.Build(rows, []string{"glab", "issue", "create", "--title", "x"}, adapt.Request{Options: map[string]string{"dry-run": "true"}}); err == nil {
		t.Fatal("adapt admitted an unsupported glab dry-run")
	}
	t.Log("dry-run: adapt row glab preview_level=unsupported refuses before execution")
}
