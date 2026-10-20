package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/provider"
)

func TestExpandIsPureSubstitution(t *testing.T) {
	got, err := provider.Expand("/m/${resource_id}?k=${idempotency_key}", map[string]string{"resource_id": "r-1", "idempotency_key": "a b"})
	if err != nil || got != "/m/r-1?k=a b" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if _, err := provider.Expand("${nope}", map[string]string{}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown placeholder: %v", err)
	}
	// Nothing is evaluated: a value that looks like a template stays text.
	got, _ = provider.Expand("${name}", map[string]string{"name": "${resource_id}"})
	if got != "${resource_id}" {
		t.Fatalf("got %q", got)
	}
}

func TestParseDeclarativeRejectsCredentialOutsideAuth(t *testing.T) {
	base := declarativeManifest("http://127.0.0.1:1", lifecycleCaps())
	var doc map[string]any
	_ = json.Unmarshal(base.Declarative, &doc)
	mutate := func(f func(doc map[string]any)) provider.Manifest {
		var d map[string]any
		_ = json.Unmarshal(base.Declarative, &d)
		f(d)
		m := base
		m.Declarative, _ = json.Marshal(d)
		return m
	}
	if _, err := provider.ParseDeclarative(base); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]provider.Manifest{
		"credential in body": mutate(func(d map[string]any) {
			d["operations"].(map[string]any)["create"].(map[string]any)["body"].(map[string]any)["key"] = "${credential}"
		}),
		"credential in path": mutate(func(d map[string]any) {
			d["operations"].(map[string]any)["list"].(map[string]any)["path"] = "/api/${credential}"
		}),
		"credential in headers": mutate(func(d map[string]any) {
			d["headers"] = map[string]string{"X-Token": "${credential}"}
		}),
		"unknown field": mutate(func(d map[string]any) { d["script"] = "echo hi" }),
		"auth without credential": mutate(func(d map[string]any) {
			d["auth"] = map[string]string{"in": "header", "name": "X", "value": "static"}
		}),
		"capability without endpoint": mutate(func(d map[string]any) {
			delete(d["operations"].(map[string]any), "stop")
		}),
		"create without list": mutate(func(d map[string]any) {
			delete(d["operations"].(map[string]any), "list")
		}),
		"bad method": mutate(func(d map[string]any) {
			d["operations"].(map[string]any)["list"].(map[string]any)["method"] = "SHELL"
		}),
	} {
		if _, err := provider.ParseDeclarative(m); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestDeclarativePlanRendersWithoutCredential(t *testing.T) {
	m := declarativeManifest("http://127.0.0.1:1", lifecycleCaps())
	a, err := provider.NewDeclarativeAdapter(m, provider.DeclarativeConfig{Credential: "sekrit", Owner: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Do(context.Background(), provider.Request{Op: provider.OpPlan, IdempotencyKey: "k1",
		Spec:          &provider.Spec{Region: "r1", Size: "s", Image: "i", Tags: []string{"extra"}},
		Authorization: &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job"}})
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Method, URL string
		Headers     map[string]string
		Body        map[string]any
	}
	if err := json.Unmarshal(res.Data, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Method != "POST" || plan.URL != "http://127.0.0.1:1/api/machines" || plan.Headers["X-Bashy-Owner"] != provider.OwnerTag("t1") {
		t.Fatalf("plan %+v", plan)
	}
	tags, _ := plan.Body["tags"].([]any)
	want := provider.LeaseTags("t1", "k1", &provider.Authorization{Kind: provider.AuthTemporary, WorkRef: "job"})
	if len(tags) != len(want)+1 || tags[0] != provider.TagLease || tags[len(tags)-1] != "extra" {
		t.Fatalf("tags %v, want %v + extra", tags, want)
	}
	if strings.Contains(string(res.Data), "sekrit") {
		t.Fatalf("plan leaks the credential: %s", res.Data)
	}
}

func TestDeclarativeAuthInQueryAndErrorMapping(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.URL.Query().Get("api_key"), r.URL.Path
		switch r.URL.Path {
		case "/machines/404":
			w.WriteHeader(404)
		case "/machines/429":
			w.WriteHeader(429)
		case "/machines/500":
			w.WriteHeader(500)
		default:
			w.WriteHeader(403)
		}
	}))
	defer srv.Close()
	doc := map[string]any{
		"base_url": srv.URL,
		"auth":     map[string]string{"in": "query", "name": "api_key", "value": "k-${credential}"},
		"resource": map[string]string{"id": "id", "status": "status", "tags": "tags"},
		"states":   map[string]string{},
		"operations": map[string]any{
			"inspect": map[string]any{"method": "GET", "path": "/machines/${resource_id}", "result": "machine"},
		},
	}
	raw, _ := json.Marshal(doc)
	m := provider.Manifest{Name: "q", Kind: provider.KindInfra, Form: provider.FormDeclarative, ProtocolVersion: provider.ProtocolVersion,
		Capabilities: []provider.Capability{"inspect"}, Declarative: raw}
	a, err := provider.NewDeclarativeAdapter(m, provider.DeclarativeConfig{Credential: "tok", Owner: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]provider.ErrorCode{"404": provider.ErrNotFound, "429": provider.ErrRateLimited, "500": provider.ErrProvider, "403": provider.ErrInvalidCredentials} {
		_, err := a.Do(context.Background(), provider.Request{Op: provider.OpInspect, ResourceID: id})
		var e *provider.Error
		if !errors.As(err, &e) || e.Code != want {
			t.Fatalf("%s: got %v, want %s", id, err, want)
		}
		if (want == provider.ErrRateLimited || want == provider.ErrProvider) != e.Retryable {
			t.Fatalf("%s: retryable=%v", id, e.Retryable)
		}
	}
	if gotKey != "k-tok" || !strings.HasPrefix(gotPath, "/machines/") {
		t.Fatalf("auth placement: key=%q path=%q", gotKey, gotPath)
	}
}
