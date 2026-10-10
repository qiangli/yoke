// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package fleetkinds

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// A resource is a SET of members held under its declared kind. A claim on it
// covers the members, not the resource's own name: a model-kind resource
// listing gpt-oss:20b refuses a direct model:gpt-oss:20b claim and use, is
// itself refused while someone holds model:gpt-oss:20b, and excludes a second
// resource sharing the member. Sprint 408's smoke: A claims the resource, B's
// `bashy claim model:gpt-oss:20b` must be refused.
func TestModelResourceClaimCoversItsMembers(t *testing.T) {
	fleetRoot(t)
	cat := fleet.New()
	for _, r := range []fleet.Resource{
		{Name: "s408dbg", Kind: "model", Members: []string{"gpt-oss:20b"}},
		{Name: "s408other", Kind: "model", Members: []string{"gpt-oss:20b", "glm-5.2"}},
	} {
		if err := cat.SaveResource(r); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	member := coord.ParseRef("model:gpt-oss:20b")

	// Resource first: the member is covered, by claim and by use.
	acquire(t, "s408dbg", "ep-m-a", "test-m-a")
	b := asHolder(t, "ep-m-b", "test-m-b")
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: member, Holder: b}); err == nil {
		t.Fatal("model claim over a resource's member admitted")
	} else if c := asConflict(t, err); c.Claim.Address() != (coord.Ref{Kind: "resource", Name: "s408dbg"}) {
		t.Fatalf("conflict names %v", c.Claim.Address())
	}
	if err := coord.Guard(ctx, b, coord.Use{Kind: "model", Name: "gpt-oss:20b", Member: "gpt-oss:20b"}); err == nil {
		t.Fatal("guard admitted a use of a resource's member")
	} else {
		asConflict(t, err)
	}
	// Two resources sharing a member exclude each other.
	if _, err := coord.AcquireRef(ctx, coord.Request{Ref: coord.ParseRef("s408other"), Holder: b}); err == nil {
		t.Fatal("second resource sharing the member admitted")
	} else {
		asConflict(t, err)
	}
	// The resource's own name is a label, not a model; another model is free.
	for _, u := range []coord.Use{
		{Kind: "model", Name: "s408dbg"},
		{Kind: "model", Name: "glm-5.2", Member: "glm-5.2"},
	} {
		if err := coord.Guard(ctx, b, u); err != nil {
			t.Errorf("%+v: refused outside the resource: %v", u, err)
		}
	}

	// Member first: the resource is refused while someone holds the model.
	a := asHolder(t, "ep-m-a", "test-m-a")
	if err := coord.ReleaseRef(ctx, coord.Ref{Kind: "resource", Name: "s408dbg"}, a, 0); err != nil {
		t.Fatal(err)
	}
	acquire(t, "model:gpt-oss:20b", "ep-m-b", "test-m-b")
	a = asHolder(t, "ep-m-a", "test-m-a")
	for _, ref := range []coord.Ref{coord.ParseRef("s408dbg"), coord.ParseRef("resource:s408other")} {
		if err := coord.Guard(ctx, a, coord.Use{Kind: ref.Kind, Name: ref.Name}); err == nil {
			t.Errorf("guard admitted %v over a held member", ref)
		} else if c := asConflict(t, err); c.Claim.Ref() != member {
			t.Errorf("%v: guard conflict names %v, want %v", ref, c.Claim.Ref(), member)
		}
		if _, err := coord.AcquireRef(ctx, coord.Request{Ref: ref, Holder: a}); err == nil {
			t.Errorf("resource %v admitted over a held member", ref)
		} else if c := asConflict(t, err); c.Claim.Ref() != member {
			t.Errorf("%v: conflict names %v, want %v", ref, c.Claim.Ref(), member)
		}
	}
}
