// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleetkinds

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

func fleetRoot(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_COORD_DIR", t.TempDir())
}

func asHolder(t *testing.T, episode, name string) principal.Ref {
	t.Helper()
	t.Setenv("BASHY_EPISODE", episode)
	// BASHY_PRINCIPAL outranks every other identity signal, including the
	// ambient values the harness injects, so the two test holders differ by
	// name as well as episode. t.Setenv restores everything afterwards.
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/"+name)
	return coord.Self()
}

func runModels(t *testing.T, args ...string) error {
	t.Helper()
	cmd := fleet.NewModelsCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

func asConflict(t *testing.T, err error) *coord.Conflict {
	t.Helper()
	var c *coord.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want *coord.Conflict", err)
	}
	return c
}

func TestEveryNounRegistersAKind(t *testing.T) {
	for _, n := range fleet.RegistryNouns() {
		k, ok := coord.LookupKind(n.Name)
		if !ok {
			t.Errorf("noun %q registers no kind", n.Name)
			continue
		}
		if k.Match != coord.MatchName || k.Domain != n.Name {
			t.Errorf("noun %q kind = %+v, want name-matched in its own domain", n.Name, k)
		}
	}
	if len(fleet.RegistryNouns()) == 0 {
		t.Fatal("no nouns in the table")
	}
}

func TestModelClaimGuardsSetAndRm(t *testing.T) {
	fleetRoot(t)
	const name = "guard-model-x"
	holderA := asHolder(t, "ep-guard-a", "test-holder-a")

	if err := runModels(t, "add", name, "--provider", "anthropic", "--kind", "subscription", "--alias", "guard-alias-x"); err != nil {
		t.Fatal(err)
	}
	if canon, aliases, ok := fleet.ResolveEntry("model", "guard-alias-x"); !ok || canon != name || len(aliases) == 0 {
		t.Fatalf("ResolveEntry(alias) = %q %q %v", canon, aliases, ok)
	}
	if _, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "model", Name: name}, Holder: holderA}); err != nil {
		t.Fatal(err)
	}

	asHolder(t, "ep-guard-b", "test-holder-b")
	// Replacing the entry rewrites it: refused.
	if err := runModels(t, "add", name, "--provider", "anthropic", "--kind", "subscription"); err == nil {
		t.Fatal("add over a claimed model admitted")
	} else {
		asConflict(t, err)
	}
	if err := runModels(t, "set", name, "--set", "display=other"); err == nil {
		t.Fatal("set on a claimed model admitted")
	} else {
		asConflict(t, err)
	}
	if err := runModels(t, "rm", name); err == nil {
		t.Fatal("rm on a claimed model admitted")
	} else {
		asConflict(t, err)
	}

	// The holder passes, and the writes land.
	asHolder(t, "ep-guard-a", "test-holder-a")
	if err := runModels(t, "set", name, "--set", "display=mine"); err != nil {
		t.Fatalf("holder refused on set: %v", err)
	}
	if err := runModels(t, "rm", name); err != nil {
		t.Fatalf("holder refused on rm: %v", err)
	}
	if _, ok := fleet.New().Model(name); ok {
		t.Fatal("rm by the holder left the entry behind")
	}
}

func TestNewNounRegistersAKind(t *testing.T) {
	fleetRoot(t)
	known := map[string]string{"w1": "w1", "dub": "w1"}
	fleet.RegisterTestNoun("widget",
		func(_ *fleet.Catalog, name string) (string, bool) {
			c, ok := known[name]
			return c, ok
		},
		func(_ *fleet.Catalog, canon string) []string {
			if canon == "w1" {
				return []string{"dub"}
			}
			return nil
		})
	defer fleet.UnregisterTestNoun("widget")

	Sync()
	k, ok := coord.LookupKind("widget")
	if !ok || k.Match != coord.MatchName || k.Domain != "widget" {
		t.Fatalf("kind = %+v ok=%v", k, ok)
	}
	holderA := asHolder(t, "ep-widget-a", "test-widget-a")
	g, err := coord.AcquireRef(context.Background(), coord.Request{Ref: coord.Ref{Kind: "widget", Name: "w1"}, Holder: holderA})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Claim.Members) != 2 || g.Claim.Members[0] != "w1" || g.Claim.Members[1] != "dub" {
		t.Fatalf("members = %v", g.Claim.Members)
	}
	asHolder(t, "ep-widget-b", "test-widget-b")
	if err := coord.Guard(context.Background(), coord.Self(), coord.Use{Kind: "widget", Name: "w1"}); !isConflictLike(err) {
		t.Fatalf("other holder admitted: %v", err)
	}
}

func isConflictLike(err error) bool {
	var c *coord.Conflict
	return errors.As(err, &c)
}
