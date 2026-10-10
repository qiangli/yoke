package resources

import (
	"context"
	"errors"
	"strings"
	"testing"

	// The rm guard reads live claims through the registry-derived providers,
	// and kind records register coord kinds at load: both need this wiring.
	_ "github.com/qiangli/yoke/pkg/fleet/fleetkinds"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

func registryRoot(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
}

func registryHolder(t *testing.T, episode, name string) {
	t.Helper()
	t.Setenv("BASHY_EPISODE", episode)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/"+name)
}

func TestResourceRegistryRoundTrip(t *testing.T) {
	registryRoot(t)
	if out, err := run(t, "kind", "add", "gpunode", "--match", "member", "--domain", "gpu"); err != nil {
		t.Fatalf("kind add: %v\n%s", err, out)
	}
	if out, err := run(t, "add", "gpu0", "--kind", "gpunode", "--title", "GPU 0", "--alias", "g0", "--guard", "weave add", "gpu:0"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if out, err := run(t, "list"); err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	} else if !strings.Contains(out, "gpu0") || !strings.Contains(out, "gpunode") || !strings.Contains(out, "gpu:0") {
		t.Fatalf("list missing the entry:\n%s", out)
	}
	if out, err := run(t, "show", "g0"); err != nil {
		t.Fatalf("show by alias: %v\n%s", err, out)
	} else if !strings.Contains(out, "kind:    gpunode") || !strings.Contains(out, "GPU 0") {
		t.Fatalf("show missing fields:\n%s", out)
	}
	if out, err := run(t, "kind", "list"); err != nil {
		t.Fatalf("kind list: %v\n%s", err, out)
	} else if !strings.Contains(out, "gpunode") || !strings.Contains(out, "member") {
		t.Fatalf("kind list missing the kind:\n%s", out)
	}
	if out, err := run(t, "kind", "show", "gpunode"); err != nil {
		t.Fatalf("kind show: %v\n%s", err, out)
	} else if !strings.Contains(out, "match:   member") {
		t.Fatalf("kind show missing fields:\n%s", out)
	}
	if out, err := run(t, "set", "gpu0", "--title", "GPU zero", "--member", "gpu:1", "--rm-alias", "g0"); err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	if out, err := run(t, "show", "gpu0", "--json"); err != nil {
		t.Fatalf("show --json: %v\n%s", err, out)
	} else if !strings.Contains(out, "GPU zero") || !strings.Contains(out, "gpu:1") || strings.Contains(out, "g0") {
		t.Fatalf("set did not land:\n%s", out)
	}
	if out, err := run(t, "rm", "gpu0"); err != nil {
		t.Fatalf("rm: %v\n%s", err, out)
	} else if !strings.Contains(out, "removed resource gpu0") {
		t.Fatalf("rm says %q", out)
	}
	if _, err := run(t, "show", "gpu0"); err == nil {
		t.Fatal("removed resource still shows")
	}
	if out, err := run(t, "kind", "rm", "gpunode"); err != nil {
		t.Fatalf("kind rm: %v\n%s", err, out)
	} else if !strings.Contains(out, "removed resource kind gpunode") {
		t.Fatalf("kind rm says %q", out)
	}
}

func TestResourceRegistryValidation(t *testing.T) {
	registryRoot(t)
	if _, err := run(t, "add", "nokind", "member"); err == nil || !strings.Contains(err.Error(), "kind is required") {
		t.Fatalf("add without --kind: err = %v", err)
	}
	if _, err := run(t, "kind", "add", "nomatch"); err == nil || !strings.Contains(err.Error(), "--match is required") {
		t.Fatalf("kind add without --match: err = %v", err)
	}
	if _, err := run(t, "kind", "add", "path", "--match", "path"); err == nil || !strings.Contains(err.Error(), "already names a registered claim kind") {
		t.Fatalf("kind add hijacking a builtin: err = %v", err)
	}
	if _, err := run(t, "show", "missing"); err == nil {
		t.Fatal("show of a missing resource admitted")
	}
	if _, err := run(t, "set", "missing", "--title", "x"); err == nil {
		t.Fatal("set of a missing resource admitted")
	}
	if _, err := run(t, "rm", "missing"); err == nil {
		t.Fatal("rm of a missing resource admitted")
	}
}

// Removing a resource refuses while someone else holds a live claim on it —
// unless --force, which drops the record but never the underlying thing.
func TestResourceRmRefusesWhileClaimed(t *testing.T) {
	registryRoot(t)
	if out, err := run(t, "add", "stage", "--kind", "path", "/w/stage"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	registryHolder(t, "ep-rm-a", "test-rm-a")
	holderA := coord.Self()
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "resource", Name: "stage"}, Holder: holderA}); err != nil {
		t.Fatal(err)
	}

	registryHolder(t, "ep-rm-b", "test-rm-b")
	if _, err := run(t, "rm", "stage"); err == nil {
		t.Fatal("rm over a live foreign claim admitted")
	} else {
		var conflict *coord.Conflict
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v, want the claim conflict", err)
		}
	}
	if out, err := run(t, "rm", "stage", "--force"); err != nil {
		t.Fatalf("rm --force: %v\n%s", err, out)
	}
	if _, err := run(t, "show", "stage"); err == nil {
		t.Fatal("forced rm left the record behind")
	}

	// The holder's own claim never blocks their rm.
	if out, err := run(t, "add", "stage", "--kind", "path", "/w/stage"); err != nil {
		t.Fatalf("re-add: %v\n%s", err, out)
	}
	registryHolder(t, "ep-rm-a", "test-rm-a")
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "resource", Name: "stage"}, Holder: coord.Self()}); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "rm", "stage"); err != nil {
		t.Fatalf("holder refused on rm: %v\n%s", err, out)
	}
}
