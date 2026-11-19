package coord

import (
	"context"
	"testing"
)

type storageAddressProvider struct{}

func (storageAddressProvider) Kind() Kind {
	return Kind{Name: "storage-address-resource", Match: MatchName}
}
func (storageAddressProvider) Exists(string) bool { return true }
func (storageAddressProvider) Members(name string) ([]string, error) {
	return []string{name}, nil
}
func (storageAddressProvider) EffectiveKind(string) (Kind, bool) {
	return Kind{Name: "storage-address-host", Match: MatchName}, true
}
func (storageAddressProvider) MapBare(name string) (Ref, bool) {
	return Ref{Kind: "storage-address-resource", Name: name}, name == "storage-address-slot"
}

func storageAddressRefs() (Ref, Ref) {
	RegisterProvider(storageAddressProvider{})
	return ParseRef("storage-address-slot"), Ref{Kind: "storage-address-host", Name: "storage-address-slot"}
}

func TestStorageAddressLeaseExcludesEqualNamedMemberBothWays(t *testing.T) {
	resource, member := storageAddressRefs()
	for _, firstResource := range []bool{true, false} {
		name := "member-first"
		first, second := member, resource
		if firstResource {
			name, first, second = "resource-first", resource, member
		}
		t.Run(name, func(t *testing.T) {
			ledger(t)
			ctx := context.Background()
			if _, err := AcquireRef(ctx, Request{Ref: first, Holder: agentA()}); err != nil {
				t.Fatal(err)
			}
			if _, err := AcquireRef(ctx, Request{Ref: second, Holder: agentB()}); !isConflict(err) {
				t.Fatalf("overlapping storage addresses admitted different holders: %v", err)
			}
		})
	}
}

func TestStorageAddressAttachedExcludesEqualNamedMemberBothWays(t *testing.T) {
	resource, member := storageAddressRefs()
	for _, firstResource := range []bool{true, false} {
		name := "member-first"
		first, second := member, resource
		if firstResource {
			name, first, second = "resource-first", resource, member
		}
		t.Run(name, func(t *testing.T) {
			dir := ledger(t)
			ctx := context.Background()
			g, l, err := AcquireAttachedRef(ctx, Request{Ref: first, Holder: agentA()})
			if err != nil {
				t.Fatal(err)
			}
			defer ReleaseAttached(dir, g.Claim, l)
			if _, err := AcquireRef(ctx, Request{Ref: second, Holder: agentB()}); !isConflict(err) {
				t.Errorf("lease bypassed an overlapping attached claim: %v", err)
			}
			if _, other, err := AcquireAttachedRef(ctx, Request{Ref: second, Holder: agentB()}); !isConflict(err) {
				if other != nil {
					_ = other.Release()
				}
				t.Errorf("attached claim bypassed an overlapping attached claim: %v", err)
			}
			if _, err := AcquireRef(ctx, Request{Ref: first, Holder: agentB()}); !isConflict(err) {
				t.Errorf("lease bypassed the same live attached storage address: %v", err)
			}
		})
	}
}

func TestStorageAddressAttachedCleanupOnlyRemovesItsOwnRecord(t *testing.T) {
	resource, member := storageAddressRefs()
	dir := ledger(t)
	ctx := context.Background()
	g, l, err := AcquireAttachedRef(ctx, Request{Ref: resource, Holder: agentA()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if _, err := AcquireRef(ctx, Request{Ref: member, Holder: agentA()}); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseAttached(dir, g.Claim, l); err != nil {
		t.Fatal(err)
	}
	b := store{dir}.backend("storage-address-resource")
	if c, err := b.Load("storage-address-resource:storage-address-slot"); err != nil || c != nil {
		t.Errorf("attached resource record survived cleanup: claim=%+v err=%v", c, err)
	}
	if c, err := b.Load(member.String()); err != nil || c == nil || !sameHolder(c.Holder, agentA()) {
		t.Errorf("cleanup removed the independent typed claim: claim=%+v err=%v", c, err)
	}
}
