package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// `infra provision` on the executable path (S472.13b item 1): a
// declarative test provider plus an httptest master. The bundle must reach
// the provider as opaque user-data, and the planted provider credential
// must never appear in any request to the master. httptest only.
func TestInfraProvisionExecutablePath(t *testing.T) {
	isolatedFleetRoot(t)
	const planted = "planted-provider-credential-QwX9"
	t.Setenv("MYPROV_API_KEY", planted)
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "owner-session-token")

	var mu sync.Mutex
	var providerBodies []string
	providerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != planted {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/machines":
			writeTestJSON(w, 200, map[string]any{"machines": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/machines":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			providerBodies = append(providerBodies, string(body))
			mu.Unlock()
			var in struct {
				Name, Region, Size, Image string
				Tags                      []string
			}
			_ = json.Unmarshal(body, &in)
			writeTestJSON(w, 202, map[string]any{"machine": map[string]any{
				"id": "host-9", "name": in.Name, "status": "running",
				"region": in.Region, "size": in.Size, "image": in.Image,
				"tags": in.Tags, "ips": []string{}, "created_at": "2026-01-01T00:00:00Z",
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer providerAPI.Close()

	var masterSeen []string
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
		masterSeen = append(masterSeen, b.String())
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer owner-session-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, 201, map[string]any{
			"satellite_id": "sat-1", "enrollment_token": "single-use-token",
			"master_public_key": "ed25519:abc", "master_url": "https://master.example",
			"artifact_url": "https://artifacts.example/sat.tgz",
			"user_data":    "#!/bin/sh\nenroll sat-1 single-use-token\n",
		})
	}))
	defer master.Close()

	doc := fmt.Sprintf(`kind: infra
name: myprov
form: declarative
protocol_version: provider/v1
capabilities: [create, list]
declarative:
  base_url: %s
  auth: {in: header, name: X-Api-Key}
  resource: {id: id, name: name, status: status, region: region, size: size, tags: tags, public_ips: ips, created_at: created_at}
  states: {new: pending, running: running, stopped: stopped, deleted: deleted}
  operations:
    create: {method: POST, path: /api/machines, result: machine, body: {name: "${name}", region: "${spec.region}", size: "${spec.size}", image: "${spec.image}", tags: "${tags}", user_data: "${spec.user_data}"}}
    list: {method: GET, path: /api/machines, result: machines}
`, providerAPI.URL)
	cat := New(WithoutCloudOverlay())
	if err := os.MkdirAll(cat.nounDir(dirInfra), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cat.nounDir(dirInfra), "myprov.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := NewInfraCmd(WithoutCloudOverlay())
	cmd.SetArgs([]string{"provision", "myprov", "--satellite", "sat-1", "--master", master.URL})
	var out strings.Builder
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.Contains(out.String(), "host-9") {
		t.Fatalf("output = %q, want the provisioned host id", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(masterSeen) == 0 {
		t.Fatal("the master saw no bundle request")
	}
	for _, s := range masterSeen {
		if strings.Contains(s, planted) {
			t.Fatalf("provider credential reached the master in:\n%s", s)
		}
	}
	foundBundle := false
	for _, b := range providerBodies {
		if strings.Contains(b, "enroll sat-1 single-use-token") {
			foundBundle = true
		}
		if strings.Contains(b, planted) {
			// The credential rides the auth header by design; it must
			// never be pasted into the body.
			t.Fatalf("provider credential leaked into the provider request body:\n%s", b)
		}
	}
	if !foundBundle {
		t.Fatalf("the enrollment bundle never reached the provider (bodies: %q)", providerBodies)
	}
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
