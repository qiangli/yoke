package cligw

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/qiangli/yoke/pkg/fleet"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRunSessionCommandAndSockets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix socket permissions")
	}
	ts := newTestServer(t, nil)
	// The broker mounts the cligw handler under /cligw in the endpoint record.
	broker := httptest.NewServer(http.StripPrefix("/cligw", ts.Handler()))
	t.Cleanup(broker.Close)
	if err := WriteEndpoint(Endpoint{BaseURL: broker.URL + "/cligw"}); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		var out bytes.Buffer
		cmd := NewCmd()
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"session"}, args...))
		err := cmd.Execute()
		return out.String(), err
	}
	open := func(label string, extra ...string) runSessionInfo {
		t.Helper()
		out, err := run(append([]string{"open", "--label", label, "--json"}, extra...)...)
		if err != nil {
			t.Fatal(err)
		}
		var info runSessionInfo
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			t.Fatal(err)
		}
		if info.Label != label || info.Token == "" || !filepath.IsAbs(info.Socket) || info.ExpiresAt.IsZero() {
			t.Fatal("open did not return a label, token, absolute socket path and expiry")
		}
		for path, want := range map[string]os.FileMode{info.Socket: 0o600, filepath.Dir(info.Socket): 0o700} {
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != want {
				t.Fatalf("%s permissions = %o, want %o", path, fi.Mode().Perm(), want)
			}
		}
		return info
	}

	scoped := open("scoped", "--model", "warm-four", "--max-requests", "3")
	if len(scoped.Models) != 1 || scoped.Models[0] != "warm-four" {
		t.Fatalf("open models = %v", scoped.Models)
	}
	other := open("band", "--band", "L4")
	if len(other.Models) != 2 || other.Socket == scoped.Socket || other.Token == scoped.Token {
		t.Fatal("band did not get its own socket, token and model list")
	}
	if _, err := run("open", "--label", "scoped", "--model", "warm-four"); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("duplicate open error = %v", err)
	}
	if _, err := run("open", "--label", "no-backend", "--band", "L5"); err == nil || !strings.Contains(err.Error(), "no backend serves band L5") {
		t.Fatalf("empty band error = %v", err)
	}

	list, err := run("list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var sessions []runSessionInfo
	if err := json.Unmarshal([]byte(list), &sessions); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].Label != "band" || sessions[1].Label != "scoped" ||
		strings.Contains(list, scoped.Token) || strings.Contains(list, other.Token) {
		t.Fatal("list did not return both sessions with credentials omitted")
	}

	client := Endpoint{Socket: scoped.Socket}.Client()
	defer client.CloseIdleConnections()
	infer := func(token, model string, status int, want string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader(fmt.Sprintf(chatBody, model)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		assertRunResponse(t, client, req, status, want)
	}
	infer(ts.Token(), "warm-four", http.StatusUnauthorized, "invalid session token")
	infer(other.Token, "warm-four", http.StatusUnauthorized, "invalid session token")
	infer(scoped.Token, "cold-four", http.StatusForbidden, "outside the session scope")
	infer(scoped.Token, "L4", http.StatusForbidden, "outside the session scope")
	infer(scoped.Token, "warm-four", http.StatusOK, "choices")
	infer(scoped.Token, "warm-four", http.StatusTooManyRequests, "request cap")

	host := ts.do(t, http.MethodGet, "/v1/models", ts.Token(), "")
	host.Body.Close()
	if host.StatusCode != http.StatusOK {
		t.Fatalf("host token status = %d", host.StatusCode)
	}
	for _, path := range []string{"/v1/models", sessionPath} {
		host = ts.do(t, http.MethodGet, path, scoped.Token, "")
		host.Body.Close()
		if host.StatusCode != http.StatusUnauthorized {
			t.Fatalf("run token on host %s status = %d", path, host.StatusCode)
		}
	}

	for range 2 {
		if _, err := run("close", "scoped"); err != nil {
			t.Fatal(err)
		}
	}
	assertRunSocketRemoved(t, scoped.Socket)
	if err := ts.Server.Close(); err != nil {
		t.Fatal(err)
	}
	assertRunSocketRemoved(t, other.Socket)

	if err := RemoveEndpoint(); err != nil {
		t.Fatal(err)
	}
	if _, err := run("open", "--label", "door-down", "--model", "warm-four"); err == nil || !strings.Contains(err.Error(), "bashy llm up") {
		t.Fatalf("stopped door error = %v", err)
	}
}

func TestRunSessionAnthropicExpiryAndForward(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix socket permissions")
	}
	ts := newTestServer(t, nil)
	expired, _, err := ts.openRunSession(t.Context(), runSessionOptions{
		Label: "expired", Models: []string{"warm-four"}, TTL: time.Nanosecond, MaxRequests: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	live, _, err := ts.openRunSession(t.Context(), runSessionOptions{
		Label: "forward", Band: "L4", TTL: time.Minute, MaxRequests: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := Endpoint{Socket: expired.Socket}.Client()
	defer client.CloseIdleConnections()
	req := anthropicSessionRequest(t, "http://localhost/v1", expired.Token, "warm-four")
	assertRunResponse(t, client, req, http.StatusUnauthorized, "session token expired")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		cmd := NewCmd()
		cmd.SetOut(writer)
		cmd.SetArgs([]string{"session", "forward", "--socket", live.Socket})
		err := cmd.ExecuteContext(ctx)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	output := bufio.NewScanner(reader)
	if !output.Scan() {
		t.Fatalf("forward did not publish a URL: %v", output.Err())
	}
	base := output.Text()
	if !strings.HasPrefix(base, "http://127.0.0.1:") || !strings.HasSuffix(base, "/v1") {
		t.Fatalf("unexpected forward URL %q", base)
	}
	client = &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	req, err = http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+live.Token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var models ModelListResponse
	readErr := json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || len(models.Data) == 0 {
		t.Fatalf("session models = %d, %v, %v", resp.StatusCode, models.Data, readErr)
	}
	for _, model := range models.Data {
		if model.XBand != 4 || model.ID == "L4+" {
			t.Fatalf("listed model outside L4: %+v", model)
		}
	}
	req, err = http.NewRequest(http.MethodPost, base+"/responses", strings.NewReader(`{"model":"small-two","input":"say ok"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+live.Token)
	assertRunResponse(t, client, req, http.StatusForbidden, "outside the session scope")
	req = anthropicSessionRequest(t, base, live.Token, "small-two")
	assertRunResponse(t, client, req, http.StatusForbidden, "outside the session scope")
	req = anthropicSessionRequest(t, base, live.Token, "L4")
	assertRunResponse(t, client, req, http.StatusOK, "content")

	ts.runSessions.close(live.Label)
	req = anthropicSessionRequest(t, base, live.Token, "L4")
	assertRunResponse(t, client, req, http.StatusBadGateway, "")
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop on cancellation")
	}
}

func TestRunSessionInFlightIsPerToken(t *testing.T) {
	const firstToken, secondToken = "first-run-test", "second-run-test"
	first := &runSession{
		info: runSessionInfo{ExpiresAt: time.Now().Add(time.Minute)},
		hash: sha256.Sum256([]byte(firstToken)), remaining: 2, limit: 1, principal: "session:first",
	}
	second := &runSession{
		info: first.info,
		hash: sha256.Sum256([]byte(secondToken)), remaining: 2, limit: 1, principal: "session:second",
	}
	entered := make(chan string, 1)
	unblock := make(chan struct{})
	defer close(unblock)
	blocker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _, _ := (&Server{}).Authorize(r)
		entered <- principal
		<-unblock
	})
	send := func(run *runSession, token string, next http.Handler) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp := httptest.NewRecorder()
		run.handler(next).ServeHTTP(resp, req)
		return resp
	}
	go send(first, firstToken, blocker)
	select {
	case principal := <-entered:
		if principal != first.principal {
			t.Fatalf("first principal = %q", principal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request did not enter")
	}
	serve := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, _, _ := (&Server{}).Authorize(r)
		_, _ = io.WriteString(w, principal)
	})
	refusal := send(first, firstToken, serve)
	if refusal.Code != http.StatusTooManyRequests || !strings.Contains(refusal.Body.String(), "in-flight cap") {
		t.Fatalf("parallel request on same session = %d %s", refusal.Code, refusal.Body)
	}
	accepted := send(second, secondToken, serve)
	if accepted.Code != http.StatusOK || accepted.Body.String() != second.principal {
		t.Fatalf("parallel request on separate session = %d %s", accepted.Code, accepted.Body)
	}
}

func anthropicSessionRequest(t *testing.T, base, token, model string) *http.Request {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"max_tokens":64,"messages":[{"role":"user","content":"say ok"}]}`, model)
	req, err := http.NewRequest(http.MethodPost, base+"/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", token)
	return req
}

func assertRunResponse(t *testing.T, client *http.Client, req *http.Request, wantStatus int, wantText string) {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != wantStatus || !strings.Contains(string(body), wantText) {
		t.Fatalf("%s = %d %s, want %d with %q", req.URL.Path, resp.StatusCode, body, wantStatus, wantText)
	}
}

func assertRunSocketRemoved(t *testing.T, socket string) {
	t.Helper()
	for _, path := range []string{socket, filepath.Dir(socket)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("session file %s remains: %v", path, err)
		}
	}
}

// A binding that shares its name with a model opens by its binding name: the
// selector grammar alone would read the name as the model and refuse it.
func TestRunSessionScopeBindingNamedLikeAModel(t *testing.T) {
	ts := newTestServer(t, nil)
	if err := ts.catalog.fleet.SaveAgent(fleet.Agent{Name: "sonnet-x", Tool: "claude", Model: "sonnet-x"}); err != nil {
		t.Fatal(err)
	}
	if sel, err := ts.catalog.ParseModelSelector("sonnet-x"); err != nil || sel.Kind != SelectorModel {
		t.Fatalf("precondition: selector %+v err=%v", sel, err)
	}
	scope, err := ts.sessionScope(t.Context(), runSessionOptions{Label: "same-name", Models: []string{"sonnet-x"}, TTL: time.Minute, MaxRequests: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.models) != 1 || scope.models[0] != "sonnet-x" {
		t.Fatalf("scope %v", scope.models)
	}
	if _, err := ts.sessionScope(t.Context(), runSessionOptions{Label: "none", Models: []string{"no-such-binding"}, TTL: time.Minute, MaxRequests: 1}); err == nil {
		t.Fatal("an unknown binding opened a scope")
	}
}
