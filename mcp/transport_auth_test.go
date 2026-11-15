package mcp

import (
	"context"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"testing"
	"time"
)

type authTransport struct{}

func (authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-Test-Auth", "approved")
	return http.DefaultTransport.RoundTrip(r)
}

func TestAuthenticatedHTTPPreservesPolicy(t *testing.T) {
	for _, effect := range []string{"destroy", "spend", "cred", "priv"} {
		for _, allow := range []bool{false, true} {
			t.Run(effect+map[bool]string{false: "-denied", true: "-approved"}[allow], func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				records := make(chan Record, 1)
				ran := false
				srv, err := BuildServer("test", "1", Options{Policy: &Policy{Allow: map[string]bool{effect: allow}, Audit: func(r Record) { records <- r }}, Registered: func() []RegisteredCommand {
					return []RegisteredCommand{{Name: "gated", Effects: []string{effect}, Run: func(context.Context, []string, string, string) (string, string, int) { ran = true; return "", "", 0 }}}
				}})
				if err != nil {
					t.Fatal(err)
				}
				auth := func(r *http.Request) bool { return r.Header.Get("X-Test-Auth") == "approved" }
				if _, _, err := ServeHTTPServerWithAuth(ctx, srv, "0.0.0.0:0", auth); err == nil {
					t.Fatal("public bind accepted")
				}
				addr, stop, err := ServeHTTPServerWithAuth(ctx, srv, "127.0.0.1:0", auth)
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				resp, err := http.Get("http://" + addr + "/mcp")
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("unauthenticated status %d", resp.StatusCode)
				}
				cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp", HTTPClient: &http.Client{Transport: authTransport{}}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer cs.Close()
				result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "gated", Arguments: map[string]any{}})
				if err != nil {
					t.Fatal(err)
				}
				record := <-records
				if ran != allow || result.IsError == allow || record.Allowed != allow || record.Command != "gated" {
					t.Fatalf("effect policy/audit changed: ran=%v error=%v record=%+v", ran, result.IsError, record)
				}
			})
		}
	}
}
