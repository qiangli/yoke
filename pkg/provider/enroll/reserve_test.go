package enroll_test

// The provisioning ORDER (S522.2 rework 3): reserve the address, ask the
// master for a bundle rendered for that endpoint, then create and attach.
//
// Order is the whole point of these tests. The first live zoo-aws came up
// and never enrolled because the first-boot script was rendered before
// anyone knew what address the host would answer on; getting the same
// calls in the wrong sequence would reproduce that exactly.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/enroll"
)

// orderingAdapter records every step it is asked to take, in order, into
// a log shared with the fake master.
type orderingAdapter struct {
	manifest provider.Manifest
	log      *eventLog
	address  string
	reused   bool
	// reserveErr, when set, is what reservation fails with.
	reserveErr error
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, s)
}

func (l *eventLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

func (a *orderingAdapter) Manifest() provider.Manifest { return a.manifest }

func (a *orderingAdapter) ReserveEndpoint(_ context.Context, spec provider.Spec) (provider.Reservation, error) {
	a.log.add("reserve:" + spec.Name)
	if a.reserveErr != nil {
		return provider.Reservation{}, a.reserveErr
	}
	return provider.Reservation{
		Name: spec.Name + "-ip", Address: a.address,
		Endpoint: provider.SslipEndpoint(a.address), Reused: a.reused,
	}, nil
}

func (a *orderingAdapter) Do(_ context.Context, req provider.Request) (provider.Result, error) {
	if req.Op != provider.OpCreate {
		return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "fixture: no %q", req.Op)
	}
	name := req.Spec.Name
	a.log.add("create:" + name)
	// A real adapter attaches the reserved address as part of finishing
	// the create; the fixture records that it would.
	a.log.add("attach:" + name + "-ip")
	return provider.Result{
		ProtocolVersion: provider.ProtocolVersion, Op: req.Op,
		Resource: &provider.Resource{
			ID: name, Name: name, State: provider.StateRunning,
			PublicIPs: []string{a.address},
		},
	}, nil
}

// orderingMaster is a fake master that records the endpoint hint it was
// asked to render the first-boot script for.
func orderingMaster(t *testing.T, log *eventLog) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SatelliteID string `json:"satellite_id"`
			Endpoint    string `json:"endpoint"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		log.add("bundle:" + body.Endpoint)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"satellite_id":      body.SatelliteID,
			"enrollment_token":  "tok-1",
			"master_public_key": "ed25519:k",
			"master_url":        "https://master.example",
			"artifact_url":      "https://artifacts.example/sat",
			// The endpoint is baked into the script, which is the whole
			// reason it has to be known before this call.
			"user_data": "#!/bin/sh\nENDPOINT=" + body.Endpoint + "\n",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func openOrdering(t *testing.T, a *orderingAdapter) *provider.Dispatcher {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	m := provider.Manifest{
		Name: "fixture-reserve", Kind: provider.KindInfra, Form: provider.FormBuiltin,
		ProtocolVersion: provider.ProtocolVersion,
		Capabilities:    []provider.Capability{"create", "list", "destroy"},
	}
	a.manifest = m
	d, err := provider.Open(context.Background(), m, provider.Options{
		Owner:       "t1",
		Exec:        provider.ExecAllowed,
		Journal:     provider.NewMemoryJournal(),
		Credentials: provider.StaticCredential("cred"),
		Builtins: map[string]provider.BuiltinFactory{
			"fixture-reserve": func(context.Context, provider.Manifest, string) (provider.Adapter, error) {
				return provider.Guard(a), nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The sequence is: reserve the address, request the bundle WITH that
// address's endpoint, create, attach. Nothing in it depends on cloud
// metadata, and the attachment cannot race first boot.
func TestProvisionReservesTheAddressBeforeTheBundle(t *testing.T) {
	log := &eventLog{}
	master := orderingMaster(t, log)
	a := &orderingAdapter{log: log, address: "203.0.113.7"}
	d := openOrdering(t, a)

	var reserved provider.Reservation
	res, err := enroll.ProvisionExecutable(context.Background(),
		enroll.Client{BaseURL: master.URL, Session: "sess"}, d,
		enroll.ProvisionRequest{
			SatelliteID:    "zoo-aws",
			Spec:           provider.Spec{Name: "zoo-aws", Region: "us-west-2"},
			IdempotencyKey: "zoo-1",
			Reserved:       func(r provider.Reservation) { reserved = r },
		})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"reserve:zoo-aws",
		"bundle:https://203-0-113-7.sslip.io",
		"create:zoo-aws",
		"attach:zoo-aws-ip",
	}
	got := log.all()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence =\n  %v\nwant\n  %v", got, want)
	}
	if reserved.Name != "zoo-aws-ip" || reserved.Address != "203.0.113.7" {
		t.Fatalf("reported reservation = %+v", reserved)
	}
	if res.Resource == nil || res.Resource.ID != "zoo-aws" {
		t.Fatalf("resource = %+v", res.Resource)
	}
}

// An explicit --endpoint is the caller's decision: nothing is reserved
// and the master is asked for exactly the endpoint that was named.
func TestProvisionWithAnExplicitEndpointReservesNothing(t *testing.T) {
	log := &eventLog{}
	master := orderingMaster(t, log)
	a := &orderingAdapter{log: log, address: "203.0.113.7"}
	d := openOrdering(t, a)

	called := false
	if _, err := enroll.ProvisionExecutable(context.Background(),
		enroll.Client{BaseURL: master.URL, Session: "sess"}, d,
		enroll.ProvisionRequest{
			SatelliteID:    "zoo-aws",
			Endpoint:       "https://sat.example.com",
			Spec:           provider.Spec{Name: "zoo-aws"},
			IdempotencyKey: "zoo-2",
			Reserved:       func(provider.Reservation) { called = true },
		}); err != nil {
		t.Fatal(err)
	}

	want := []string{"bundle:https://sat.example.com", "create:zoo-aws", "attach:zoo-aws-ip"}
	if got := log.all(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("sequence = %v, want %v", got, want)
	}
	if called {
		t.Fatal("Reserved was called for a caller-named endpoint")
	}
}

// A reservation that fails stops everything: a bundle rendered for an
// endpoint the host will not answer on installs a satellite that silently
// never enrolls, which is worse than a failed provision.
func TestProvisionStopsWhenTheAddressCannotBeReserved(t *testing.T) {
	log := &eventLog{}
	master := orderingMaster(t, log)
	a := &orderingAdapter{
		log: log, address: "203.0.113.7",
		reserveErr: &provider.Error{Code: provider.ErrNotOwned, Message: "held by another instance"},
	}
	d := openOrdering(t, a)

	_, err := enroll.ProvisionExecutable(context.Background(),
		enroll.Client{BaseURL: master.URL, Session: "sess"}, d,
		enroll.ProvisionRequest{
			SatelliteID: "zoo-aws", Spec: provider.Spec{Name: "zoo-aws"},
			IdempotencyKey: "zoo-3",
		})
	if err == nil {
		t.Fatal("a failed reservation must fail the provision")
	}
	if got := log.all(); len(got) != 1 || got[0] != "reserve:zoo-aws" {
		t.Fatalf("sequence = %v; nothing may follow a failed reservation", got)
	}
}

// A replayed provision reuses the address it already holds and says so,
// rather than allocating a second one and changing the endpoint.
func TestProvisionReportsAReusedReservation(t *testing.T) {
	log := &eventLog{}
	master := orderingMaster(t, log)
	a := &orderingAdapter{log: log, address: "203.0.113.7", reused: true}
	d := openOrdering(t, a)

	var lines []string
	var reserved provider.Reservation
	if _, err := enroll.ProvisionExecutable(context.Background(),
		enroll.Client{BaseURL: master.URL, Session: "sess"}, d,
		enroll.ProvisionRequest{
			SatelliteID: "zoo-aws", Spec: provider.Spec{Name: "zoo-aws"},
			IdempotencyKey: "zoo-4",
			Progress:       func(s string) { lines = append(lines, s) },
			Reserved:       func(r provider.Reservation) { reserved = r },
		}); err != nil {
		t.Fatal(err)
	}
	if !reserved.Reused {
		t.Fatalf("reservation = %+v, want reused", reserved)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"reusing stable address 203.0.113.7", "https://203-0-113-7.sslip.io"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("progress does not report %q:\n%s", want, joined)
		}
	}
}
