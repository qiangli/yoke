package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

// The executable-plugin tests exec this test binary as the plugin (never a
// shell, never the bashy binary): with pluginEnv set, TestMain serves one
// request on stdin/stdout and exits.
const (
	pluginEnv = "PROVIDER_TEST_PLUGIN"
	markerEnv = "PROVIDER_TEST_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(pluginEnv) == "fixture" {
		fixturePluginMain() // fixture_test.go: the stateful executable fixture
	}
	if os.Getenv(pluginEnv) == "echo" {
		if p := os.Getenv(markerEnv); p != "" {
			_ = os.WriteFile(p, []byte("ran"), 0o600)
		}
		os.Stderr.WriteString("echo plugin: diagnostics go to stderr\n")
		if err := provider.Serve(context.Background(), echoAdapter{allOps()}, os.Stdin, os.Stdout); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func allOps() []provider.Capability {
	var caps []provider.Capability
	for _, op := range provider.Ops() {
		caps = append(caps, provider.Capability(op))
	}
	return caps
}

func manifest(form provider.Form, caps []provider.Capability) provider.Manifest {
	return provider.Manifest{
		Name: "echo", Kind: provider.KindInfra, Form: form,
		ProtocolVersion: provider.ProtocolVersion, Capabilities: caps,
	}
}

// echoAdapter answers every request with the request itself as Data.
type echoAdapter struct{ caps []provider.Capability }

func (e echoAdapter) Manifest() provider.Manifest { return manifest(provider.FormBuiltin, e.caps) }

func (e echoAdapter) Do(_ context.Context, req provider.Request) (provider.Result, error) {
	data, _ := json.Marshal(req)
	return provider.Result{
		ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: data,
		Resource: &provider.Resource{ID: "r-1", State: provider.StateRunning, RawStatus: "active"},
	}, nil
}

func execAdapter(t *testing.T, caps []provider.Capability) (provider.Adapter, string, *bytes.Buffer) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	m := manifest(provider.FormExecutable, caps)
	m.Argv = []string{os.Args[0], "-test.run=^$"}
	var stderr bytes.Buffer
	a, err := provider.NewExecAdapter(m, provider.ExecOptions{
		Stderr: &stderr,
		Env:    append(os.Environ(), pluginEnv+"=echo", markerEnv+"="+marker),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, marker, &stderr
}

func code(err error) provider.ErrorCode {
	var e *provider.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// sampleRequest returns a valid, fully populated request for op.
func sampleRequest(op provider.Op) provider.Request {
	req := provider.Request{ProtocolVersion: provider.ProtocolVersion, Op: op}
	if op.Mutating() {
		req.IdempotencyKey = "key-" + string(op)
	}
	switch op {
	case provider.OpInspect, provider.OpStart, provider.OpStop, provider.OpReboot, provider.OpDestroy:
		req.ResourceID = "12345"
	case provider.OpResize:
		req.ResourceID = "12345"
		req.Spec = &provider.Spec{Size: "s-2vcpu-4gb"}
	case provider.OpCreate, provider.OpPlan:
		req.Spec = &provider.Spec{
			Name: "sat-1", Region: "r1", Size: "s-1vcpu-1gb", Image: "img",
			Tags: []string{"extra"}, Labels: map[string]string{"role": "satellite"},
			UserData: []byte("#cloud-config\n\x00opaque\xff"),
		}
		req.Authorization = &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), WorkRef: "story-1"}
	}
	if op == provider.OpDestroy {
		req.Authorization = &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: "12345", IdempotencyKey: req.IdempotencyKey}
	}
	return req
}

func TestSchemaRoundTripEveryOp(t *testing.T) {
	for _, op := range provider.Ops() {
		req := sampleRequest(op)
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var back provider.Request
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(req, back) {
			t.Errorf("%s request round trip:\n got %+v\nwant %+v", op, back, req)
		}
		res := provider.Result{
			ProtocolVersion: provider.ProtocolVersion, Op: op,
			Resource:  &provider.Resource{ID: "12345", Name: "n", State: provider.StateStopped, RawStatus: "off", Tags: []string{"a"}, PublicIPs: []string{"192.0.2.1"}, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			Resources: []provider.Resource{{ID: "1", State: provider.StateRunning}},
			Cost:      &provider.CostReport{Ongoing: true, Retained: []string{"volume:v1"}, Note: "n"},
			Data:      json.RawMessage(`{"k":"v"}`),
			Error:     &provider.Error{Code: provider.ErrTimeout, Message: "m", Retryable: true},
		}
		b, err = json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		var rback provider.Result
		if err := json.Unmarshal(b, &rback); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(res, rback) {
			t.Errorf("%s result round trip:\n got %+v\nwant %+v", op, rback, res)
		}
	}
}

// TestExecSchemaRoundTripEveryOp drives every op through a real plugin
// process over stdin/stdout: the plugin must see exactly the request sent.
func TestExecSchemaRoundTripEveryOp(t *testing.T) {
	a, _, stderr := execAdapter(t, allOps())
	for _, op := range provider.Ops() {
		req := sampleRequest(op)
		res, err := a.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if res.Op != op || res.ProtocolVersion != provider.ProtocolVersion || res.Resource == nil || res.Resource.ID != "r-1" {
			t.Fatalf("%s: unexpected result %+v", op, res)
		}
		var seen provider.Request
		if err := json.Unmarshal(res.Data, &seen); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(seen, req) {
			t.Errorf("%s: plugin saw\n %+v\nwant\n %+v", op, seen, req)
		}
	}
	if !strings.Contains(stderr.String(), "diagnostics go to stderr") {
		t.Errorf("plugin stderr not forwarded: %q", stderr.String())
	}
}

func TestUnsupportedOperationIsExplicit(t *testing.T) {
	var caps []provider.Capability
	for _, c := range allOps() {
		if c != provider.Capability(provider.OpStop) {
			caps = append(caps, c)
		}
	}
	a, marker, _ := execAdapter(t, caps)
	res, err := a.Do(context.Background(), sampleRequest(provider.OpStop))
	if code(err) != provider.ErrUnsupportedCapability || res.Error == nil || res.Error.Code != provider.ErrUnsupportedCapability {
		t.Fatalf("got err=%v result=%+v, want unsupported_capability", err, res)
	}
	if res.Op != provider.OpStop || res.Resource != nil {
		t.Fatalf("unsupported op answered with something else: %+v", res)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("adapter argv was executed for an undeclared op (marker: %v)", statErr)
	}
	// Unknown ops are unsupported too, and also never executed.
	if _, err := a.Do(context.Background(), provider.Request{Op: "hibernate"}); code(err) != provider.ErrUnsupportedCapability {
		t.Fatalf("unknown op: %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("adapter argv was executed for an unknown op")
	}
}

// substituter answers a stop with a destroy.
type substituter struct{}

func (substituter) Manifest() provider.Manifest { return manifest(provider.FormBuiltin, allOps()) }
func (substituter) Do(context.Context, provider.Request) (provider.Result, error) {
	return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: provider.OpDestroy}, nil
}

func TestGuardRefusesOperationSubstitution(t *testing.T) {
	_, err := provider.Guard(substituter{}).Do(context.Background(), sampleRequest(provider.OpStop))
	if code(err) != provider.ErrProtocolMismatch {
		t.Fatalf("got %v, want protocol_mismatch", err)
	}
}

func TestMutatingOpNeedsIdempotencyKey(t *testing.T) {
	for _, op := range provider.Ops() {
		if !op.Mutating() {
			continue
		}
		req := sampleRequest(op)
		req.IdempotencyKey = ""
		if _, err := provider.Guard(echoAdapter{allOps()}).Do(context.Background(), req); code(err) != provider.ErrInvalidRequest {
			t.Errorf("%s without key: %v", op, err)
		}
	}
}

func TestValidateManifest(t *testing.T) {
	good := manifest(provider.FormExecutable, allOps())
	good.Argv = []string{"/bin/plugin"}
	if err := provider.ValidateManifest(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*provider.Manifest){
		"kind":     func(m *provider.Manifest) { m.Kind = "provider" },
		"protocol": func(m *provider.Manifest) { m.ProtocolVersion = "provider/v0" },
		"argv":     func(m *provider.Manifest) { m.Argv = nil },
		"cap":      func(m *provider.Manifest) { m.Capabilities = append(m.Capabilities, "teleport") },
		"name":     func(m *provider.Manifest) { m.Name = "" },
	}
	for name, mut := range cases {
		m := good
		m.Capabilities = append([]provider.Capability(nil), good.Capabilities...)
		mut(&m)
		if err := provider.ValidateManifest(m); err == nil {
			t.Errorf("%s: invalid manifest accepted", name)
		}
	}
}

func TestLeaseTags(t *testing.T) {
	exp := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	tags := provider.LeaseTags("o1", "k", &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: exp, WorkRef: "w/1"})
	if !provider.Owned(tags, "o1") || provider.Owned(tags, "o2") {
		t.Fatalf("ownership: %v", tags)
	}
	if got, ok := provider.DeadlineFromTags(append(tags, provider.DeadlineTag(exp.Add(-time.Hour)))); !ok || !got.Equal(exp) {
		t.Fatalf("deadline %v %v", got, ok)
	}
	for _, tag := range tags {
		if len(tag) > 255 || strings.ContainsAny(tag, " /.") {
			t.Errorf("tag %q is not provider-safe", tag)
		}
	}
	if p := provider.LeaseTags("o1", "k", nil); len(p) != 3 {
		t.Fatalf("persistent tags: %v", p)
	}
}

// ---- A6 destroy authorization, end to end against the DO fake ----

func newDO(t *testing.T, fake *fakedo.Server) provider.Adapter {
	t.Helper()
	a, err := digitalocean.New(digitalocean.Config{BaseURL: fake.URL, Token: fakedo.TestToken, Owner: "t1", PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func create(t *testing.T, a provider.Adapter, key string, auth *provider.Authorization) string {
	t.Helper()
	res, err := a.Do(context.Background(), provider.Request{
		Op: provider.OpCreate, IdempotencyKey: key, Authorization: auth,
		Spec: &provider.Spec{Name: "sat", Region: "r1", Size: "s-1vcpu-1gb", Image: "img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Resource.ID
}

func TestDestroyWithoutConfirmationRefused(t *testing.T) {
	fake := fakedo.New(t)
	a := newDO(t, fake)
	id := create(t, a, "create-1", nil)
	other := create(t, a, "create-2", nil)
	before := fake.RequestCount()

	for name, auth := range map[string]*provider.Authorization{
		"none":           nil,
		"other resource": {Kind: provider.AuthConfirmation, ResourceID: other, IdempotencyKey: "destroy-1"},
		"other key":      {Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: "destroy-0"},
		"unknown kind":   {Kind: "maybe", ResourceID: id, IdempotencyKey: "destroy-1"},
	} {
		res, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "destroy-1", ResourceID: id, Authorization: auth})
		if code(err) != provider.ErrConfirmationRequired {
			t.Fatalf("%s: got %v, want confirmation_required", name, err)
		}
		if res.Cost == nil || !res.Cost.Ongoing {
			t.Fatalf("%s: refused destroy must report ongoing cost: %+v", name, res.Cost)
		}
	}
	if fake.RequestCount() != before {
		t.Fatalf("refused destroys reached the provider: %d requests", fake.RequestCount()-before)
	}

	// A temporary authorization on a persistent resource is refused too
	// (this one needs the resource's tags, but still never deletes).
	_, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "destroy-1", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary}})
	if code(err) != provider.ErrConfirmationRequired {
		t.Fatalf("temporary auth on persistent resource: %v", err)
	}
	if n := fake.DeleteCount(); n != 0 {
		t.Fatalf("DeleteCount = %d, want 0", n)
	}

	// A confirmation bound to this resource and this key goes through.
	_, err = a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "destroy-1", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthConfirmation, ResourceID: id, IdempotencyKey: "destroy-1"}})
	if err != nil || fake.DeleteCount() != 1 {
		t.Fatalf("confirmed destroy: err=%v deletes=%d", err, fake.DeleteCount())
	}
}

// TestTemporaryResourceDestroyNoPromptAdapter is the adapter-level half:
// the DO adapter honors a temporary authorization on a resource created as
// temporary. The dispatcher-level TestTemporaryResourceDestroyNoPrompt
// (dispatcher_test.go) covers the journaled TTL sweep across all forms.
func TestTemporaryResourceDestroyNoPromptAdapter(t *testing.T) {
	fake := fakedo.New(t)
	a := newDO(t, fake)
	ttl := &provider.Authorization{Kind: provider.AuthTemporary, ExpiresAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	id := create(t, a, "tmp-1", ttl)

	res, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "tmp-destroy-1", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Resource == nil || res.Resource.State != provider.StateDeleted {
		t.Fatalf("result %+v", res)
	}
	if n := fake.DeleteCount(); n != 1 {
		t.Fatalf("DeleteCount = %d, want 1", n)
	}
}

func TestWorkBoundTemporaryAuthorizationMustMatch(t *testing.T) {
	fake := fakedo.New(t)
	a := newDO(t, fake)
	id := create(t, a, "w-1", &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job-a"})
	_, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job-b"}})
	if code(err) != provider.ErrConfirmationRequired || fake.DeleteCount() != 0 {
		t.Fatalf("mismatched work ref: err=%v deletes=%d", err, fake.DeleteCount())
	}
	if _, err := a.Do(context.Background(), provider.Request{Op: provider.OpDestroy, IdempotencyKey: "d", ResourceID: id,
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job-a"}}); err != nil || fake.DeleteCount() != 1 {
		t.Fatalf("matching work ref: err=%v deletes=%d", err, fake.DeleteCount())
	}
}

func TestServeDecodeErrorIsAResult(t *testing.T) {
	var out bytes.Buffer
	if err := provider.Serve(context.Background(), echoAdapter{allOps()}, strings.NewReader("{not json"), &out); err != nil {
		t.Fatal(err)
	}
	var r provider.Result
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || r.Error == nil || r.Error.Code != provider.ErrInvalidRequest {
		t.Fatalf("got %q", out.String())
	}
}
