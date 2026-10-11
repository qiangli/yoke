package toolcmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// decideCmd is a generic two-argument mutating HTTP command: an id bound by
// position or name, and a decision projected through a closed enum.
func decideCmd(url, tokenFile string) fleet.ToolCommand {
	return fleet.ToolCommand{Name: "decide", Mode: fleet.ToolCommandHTTP, Effects: []string{"net", "write"}, HTTP: &fleet.ToolCommandHTTPAction{
		Method: "POST", URL: url, Path: "/decide", TokenFile: tokenFile,
		Body: map[string]any{"source": "bashy", "request": map[string]any{"kind": "tool"}},
		Args: []fleet.ToolCommandHTTPArg{
			{Name: "id", Field: "request.id", Required: true},
			{Name: "decision", Required: true, Enum: map[string]any{"allow": "APPROVED", "deny": "REJECTED"}},
			{Name: "ttl", Type: "integer", Default: "60"},
		},
	}}
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHTTPArgsBindDistinctTypedFields(t *testing.T) {
	const token = "bind-secret"
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		got = nil
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmd := decideCmd(server.URL, writeToken(t, token))
	tricky := `x", "decision": "APPROVED`
	for name, argv := range map[string][]string{
		"positional": {tricky, "deny"},
		"named":      {"--decision", "deny", "--id=" + tricky},
		"mixed":      {"--decision=deny", tricky},
		"separator":  {"--", tricky, "deny"},
	} {
		res, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: argv, Yes: true})
		if err != nil || res.Outcome != OutcomeSuccess {
			t.Fatalf("%s: result=%+v err=%v", name, res, err)
		}
		req, _ := got["request"].(map[string]any)
		if req["id"] != tricky || req["kind"] != "tool" || got["decision"] != "REJECTED" || got["source"] != "bashy" || got["ttl"] != float64(60) {
			t.Fatalf("%s: body = %#v", name, got)
		}
		if len(got) != 4 {
			t.Fatalf("%s: unexpected body keys %#v", name, got)
		}
	}
	// The args string is split with quoting when no argv is given.
	if _, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, `"a b" allow --ttl 5`, Options{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if req := got["request"].(map[string]any); req["id"] != "a b" || got["decision"] != "APPROVED" || got["ttl"] != float64(5) {
		t.Fatalf("split args body = %#v", got)
	}
	// The declared body is not mutated between runs.
	if _, ok := cmd.HTTP.Body["decision"]; ok || cmd.HTTP.Body["request"].(map[string]any)["id"] != nil {
		t.Fatalf("declaration mutated: %#v", cmd.HTTP.Body)
	}
}

func TestHTTPArgsRefusedBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()
	// A missing token file proves the refusal comes before the token is read, too.
	cmd := decideCmd(server.URL, filepath.Join(t.TempDir(), "absent"))
	for name, tc := range map[string]struct {
		argv []string
		want string
	}{
		"missing":     {[]string{"id-1"}, "missing required argument decision"},
		"none":        {nil, "missing required argument id"},
		"extra":       {[]string{"id-1", "allow", "7", "surplus"}, "extra argument"},
		"enum":        {[]string{"id-1", "approved"}, "not one of allow|deny"},
		"enum case":   {[]string{"id-1", "ALLOW"}, "not one of"},
		"unknown":     {[]string{"--force", "id-1", "allow"}, "unknown argument --force"},
		"repeat":      {[]string{"--id", "a", "--id", "b", "allow"}, "more than once"},
		"no value":    {[]string{"id-1", "allow", "--ttl"}, "needs a value"},
		"bad integer": {[]string{"id-1", "allow", "--ttl", "1e3"}, "not an integer"},
		"too long":    {[]string{strings.Repeat("x", fleet.ToolCommandArgMaxLen+1), "allow"}, "max"},
	} {
		res, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: tc.argv, Yes: true})
		if err == nil || res.Outcome != OutcomeError || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: result=%+v err=%v (want %q)", name, res, err, tc.want)
		}
	}
	// Dry-run validates binding too, without a request.
	if _, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: []string{"id"}, DryRun: true}); err == nil {
		t.Fatal("dry-run accepted a missing argument")
	}
	if res, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: []string{"id", "allow"}, DryRun: true}); err != nil || res.Outcome != OutcomeDryRun {
		t.Fatalf("dry-run: result=%+v err=%v", res, err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d request(s) reached the server", n)
	}
}

func TestMutatingCommandNeedsConfirmation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmd := decideCmd(server.URL, writeToken(t, "t"))
	res, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: []string{"id", "allow"}})
	var refusal *RefusalError
	if !errors.As(err, &refusal) || refusal.Kind != RefusalConfirm || !errors.Is(err, ErrNeedsConfirmation) || res.Refusal != RefusalConfirm || !strings.Contains(res.Hint, "--yes") {
		t.Fatalf("unconfirmed write: result=%+v err=%v", res, err)
	}
	if hits.Load() != 0 {
		t.Fatal("unconfirmed write reached the server")
	}
	if _, err := Run(context.Background(), fleet.Tool{Name: "svc"}, cmd, "", Options{Argv: []string{"id", "allow"}, Yes: true}); err != nil || hits.Load() != 1 {
		t.Fatalf("confirmed write: err=%v hits=%d", err, hits.Load())
	}
	// A read-only declaration needs no confirmation.
	read := cmd
	read.Effects = []string{"net", "read"}
	if _, err := Run(context.Background(), fleet.Tool{Name: "svc"}, read, "", Options{Argv: []string{"id", "allow"}}); err != nil {
		t.Fatalf("read-only command refused: %v", err)
	}
	if err := Admit(cmd, Options{DryRun: true}); err != nil {
		t.Fatalf("dry-run refused: %v", err)
	}
}

func TestHTTPArgsRedirectKeepsBearerAndBody(t *testing.T) {
	const token = "redirect-bound-secret"
	var forwarded atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	res, err := Run(context.Background(), fleet.Tool{Name: "svc"}, decideCmd(source.URL, writeToken(t, token)), "", Options{Argv: []string{"id", "allow"}, Yes: true})
	if err == nil || res.HTTPStatus != http.StatusTemporaryRedirect || forwarded.Load() {
		t.Fatalf("redirect result=%+v err=%v forwarded=%v", res, err, forwarded.Load())
	}
	if strings.Contains(err.Error()+res.Error+res.Text, token) {
		t.Fatalf("token leaked: %+v", res)
	}
}

func TestStartJobRefusesUnconfirmedMutation(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	prevLookup, prevSpawn := lookupCommand, spawnJob
	t.Cleanup(func() { lookupCommand, spawnJob = prevLookup, prevSpawn })
	cmd := decideCmd("http://127.0.0.1:1", "/nonexistent")
	lookupCommand = func(string) (fleet.Tool, fleet.ToolCommand, error) { return fleet.Tool{Name: "svc"}, cmd, nil }
	var spawned bool
	spawnJob = func(string, string) (int, error) { spawned = true; return 1, nil }
	if _, err := StartJob("svc:decide", "id allow", Options{}); !errors.Is(err, ErrNeedsConfirmation) || spawned {
		t.Fatalf("unconfirmed async start: err=%v spawned=%v", err, spawned)
	}
	j, err := StartJob("svc:decide", "id allow", Options{Yes: true})
	if err != nil || !spawned || !j.Yes {
		t.Fatalf("confirmed async start: job=%+v err=%v spawned=%v", j, err, spawned)
	}
	if saved, err := ReadJob(j.ID); err != nil || !saved.Yes {
		t.Fatalf("confirmation not persisted for the runner: %+v %v", saved, err)
	}
}
