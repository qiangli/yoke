// Red tests for S472.13b item 1 (OSS executable path): enrollment-bundle
// provisioning against the public dhnt.io API. httptest only, no network,
// no exec of the bashy self binary; HOME/BASHY_HOME isolated per test.
package enroll_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/enroll"
)

// countingAdapter is the fixture provider behind the dispatcher: it records
// every call and answers create/destroy from memory.
type countingAdapter struct {
	manifest provider.Manifest
	mu       sync.Mutex
	calls    int
	creates  int
	destroys int
}

func (a *countingAdapter) Manifest() provider.Manifest { return a.manifest }

func (a *countingAdapter) Do(_ context.Context, req provider.Request) (provider.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	switch req.Op {
	case provider.OpCreate:
		a.creates++
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion, Op: req.Op,
			Resource: &provider.Resource{ID: "host-1", Name: "sat", State: provider.StateRunning},
		}, nil
	case provider.OpDestroy:
		a.destroys++
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion, Op: req.Op,
			Resource: &provider.Resource{ID: req.ResourceID, State: provider.StateDeleted},
		}, nil
	case provider.OpList:
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op}, nil
	}
	return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "fixture: no %q", req.Op)
}

func (a *countingAdapter) counts() (calls, creates, destroys int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls, a.creates, a.destroys
}

func fixtureManifest() provider.Manifest {
	return provider.Manifest{
		Name: "fixture-exec", Kind: provider.KindInfra, Form: provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Capabilities:    []provider.Capability{"create", "list", "destroy"},
	}
}

func openDispatcher(t *testing.T, a *countingAdapter, credential string) *provider.Dispatcher {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	d, err := provider.Open(context.Background(), fixtureManifest(), provider.Options{
		Owner:       "t1",
		Exec:        provider.ExecAllowed, // executable path runs on user-owned compute
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential(credential),
		Builtins: map[string]provider.BuiltinFactory{
			"fixture-exec": func(context.Context, provider.Manifest, string) (provider.Adapter, error) {
				return provider.Guard(a), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// First red test (S472.13b): an httptest master that returns 503 for the
// enrollment-bundle request must produce error kind master_unavailable, and
// the fixture adapter's invocation count must be 0 (no provider call).
func TestProvisionBlockedWhenMasterUnavailable(t *testing.T) {
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"try again later"}`))
	}))
	defer master.Close()

	fixture := &countingAdapter{manifest: fixtureManifest()}
	d := openDispatcher(t, fixture, "fixture-credential")
	var progress []string
	_, err := enroll.ProvisionExecutable(context.Background(), enroll.Client{
		BaseURL: master.URL, Session: "owner-session",
	}, d, enroll.ProvisionRequest{
		SatelliteID:    "sat-1",
		Spec:           provider.Spec{Name: "sat", Region: "r1", Size: "s-1", Image: "img"},
		IdempotencyKey: "boot-1",
		Progress:       func(s string) { progress = append(progress, s) },
	})
	if enroll.KindOf(err) != enroll.KindMasterUnavailable {
		t.Fatalf("err = %v, want kind %q", err, enroll.KindMasterUnavailable)
	}
	if calls, _, _ := fixture.counts(); calls != 0 {
		t.Fatalf("fixture adapter invocations = %d, want 0 (no provider call)", calls)
	}
	if len(progress) == 0 {
		t.Fatal("provision reported no progress")
	}
}

// The executable path must never send the provider credential to the
// master: the recorded master requests must contain zero hits for the
// planted credential.
func TestNoProviderCredentialSentToMaster(t *testing.T) {
	const planted = "planted-provider-credential-ZqX1"
	var mu sync.Mutex
	var seen []string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b strings.Builder
		b.WriteString(r.Method + " " + r.URL.RequestURI() + "\n")
		for k, vs := range r.Header {
			for _, v := range vs {
				b.WriteString(k + ": " + v + "\n")
			}
		}
		b.Write(body)
		mu.Lock()
		seen = append(seen, b.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"satellite_id":"sat-1","enrollment_token":"single-use-token",` +
			`"master_public_key":"ed25519:abc","master_url":"https://master.example","artifact_url":"https://artifacts.example/sat.tgz"}`))
	}))
	defer master.Close()

	fixture := &countingAdapter{manifest: fixtureManifest()}
	d := openDispatcher(t, fixture, planted)
	res, err := enroll.ProvisionExecutable(context.Background(), enroll.Client{
		BaseURL: master.URL, Session: "owner-session",
	}, d, enroll.ProvisionRequest{
		SatelliteID:    "sat-1",
		Spec:           provider.Spec{Name: "sat", Region: "r1", Size: "s-1", Image: "img"},
		IdempotencyKey: "boot-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource == nil || res.Resource.ID == "" {
		t.Fatalf("result = %+v, want a created resource", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the master saw no request")
	}
	for _, s := range seen {
		if strings.Contains(s, planted) {
			t.Fatalf("provider credential reached the master in:\n%s", s)
		}
	}
}

// Destroy on the executable path goes through the dispatcher's A6
// per-operation confirmation: without it the destroy is refused and the
// fixture adapter's destroy count is 0.
func TestExecutablePathDestroyRequiresConfirmation(t *testing.T) {
	fixture := &countingAdapter{manifest: fixtureManifest()}
	d := openDispatcher(t, fixture, "fixture-credential")
	_, err := enroll.DestroyExecutable(context.Background(), d, enroll.DestroyRequest{
		ResourceID: "host-1", IdempotencyKey: "d-1",
	})
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Code != provider.ErrConfirmationRequired {
		t.Fatalf("err = %v, want confirmation_required", err)
	}
	if _, _, destroys := fixture.counts(); destroys != 0 {
		t.Fatalf("fixture adapter destroys = %d, want 0", destroys)
	}
}
