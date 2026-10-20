package digitalocean_test

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func newAdapter(t *testing.T, fake *fakedo.Server) provider.Adapter {
	t.Helper()
	a, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: fakedo.TestToken, Owner: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testSpec() *provider.Spec {
	return &provider.Spec{Name: "sat-1", Region: "r1", Size: "s-1vcpu-1gb", Image: "ubuntu-24-04-x64"}
}

func TestCreateIdempotencyKeyReplay(t *testing.T) {
	fake := fakedo.New(t)
	a := newAdapter(t, fake)
	req := provider.Request{Op: provider.OpCreate, IdempotencyKey: "k-1", Spec: testSpec()}
	r1, err := a.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := a.Do(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Resource == nil || r2.Resource == nil || r1.Resource.ID == "" || r1.Resource.ID != r2.Resource.ID {
		t.Fatalf("replay returned a different resource: %+v vs %+v", r1.Resource, r2.Resource)
	}
	if n := fake.CreateCount(); n != 1 {
		t.Fatalf("CreateCount = %d, want 1", n)
	}
}
