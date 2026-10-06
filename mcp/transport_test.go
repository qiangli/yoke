package mcp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func transportToolNames(t *testing.T, ctx context.Context, session *mcpsdk.ClientSession) []string {
	t.Helper()
	if session.InitializeResult() == nil {
		t.Fatal("missing initialize result")
	}
	info, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "server_info"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		AllowedEffects []string `json:"allowed_effects"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(identity.AllowedEffects, []string{"destroy"}) {
		t.Fatalf("transport lost policy options: %s", data)
	}
	var names []string
	params := &mcpsdk.ListToolsParams{}
	for {
		result, err := session.ListTools(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range result.Tools {
			names = append(names, tool.Name)
		}
		if result.NextCursor == "" {
			break
		}
		params.Cursor = result.NextCursor
	}
	slices.Sort(names)
	if !slices.Contains(names, "list_tools") || !slices.Contains(names, "run_tool") {
		t.Fatalf("missing compatibility tools: %v", names)
	}
	return names
}

// This test owns process stdio for its duration; it must not run in parallel.
func TestTransportStdioHTTPParity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	serverIn, clientOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer serverIn.Close()
	defer clientOut.Close()
	clientIn, serverOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer clientIn.Close()
	defer serverOut.Close()
	stdin, stdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = serverIn, serverOut
	defer func() { os.Stdin, os.Stdout = stdin, stdout }()
	opts := Options{Policy: &Policy{Allow: map[string]bool{"destroy": true}, Audit: func(Record) {}}}
	finished := make(chan error, 1)
	go func() { finished <- ServeStdioWithOptions(ctx, "transport-test", "1", opts) }()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "transport-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	stdioNames := transportToolNames(t, ctx, session)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("stdio shutdown: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("stdio shutdown timed out")
	}

	for _, listen := range []string{"127.0.0.1:0", "[::1]:0", "localhost:0", ""} {
		t.Run("http_"+listen, func(t *testing.T) {
			serverCtx, stop := context.WithCancel(ctx)
			defer stop()
			addr, shutdown, err := ServeHTTPWithShutdown(serverCtx, "transport-test", "1", opts, listen)
			if err != nil {
				t.Fatal(err)
			}
			defer shutdown()
			host, _, err := net.SplitHostPort(addr)
			if err != nil || !net.ParseIP(host).IsLoopback() {
				t.Fatalf("non-loopback bound address %q: %v", addr, err)
			}
			endpoint := "http://" + addr
			httpClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
			defer httpClient.CloseIdleConnections()
			cs, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: endpoint + "/mcp", HTTPClient: httpClient}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			if names := transportToolNames(t, ctx, cs); !slices.Equal(names, stdioNames) {
				t.Fatalf("HTTP tools %v != stdio tools %v", names, stdioNames)
			}
			for _, path := range []string{"/sse", "/", "/mcp"} {
				response, err := httpClient.Get(endpoint + path)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				want := http.StatusNotFound
				if path == "/mcp" {
					want = http.StatusMethodNotAllowed
				}
				if response.StatusCode != want {
					t.Errorf("GET %s: %d, want %d", path, response.StatusCode, want)
				}
				if id := response.Header.Get("Mcp-Session-Id"); id != "" {
					t.Errorf("stateless response has session ID %q", id)
				}
			}
			stop()
			done := make(chan error, 1)
			go func() { done <- shutdown() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("HTTP shutdown timed out")
			}
			if err := shutdown(); err != nil {
				t.Fatalf("repeated shutdown: %v", err)
			}
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				conn.Close()
				t.Fatal("listener remains open after shutdown")
			}
		})
	}
}

func TestServeHTTPRejectsNonLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", "192.0.2.1:1", "example.com:80", "[::]:0", ":0"} {
		t.Run(listen, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			addr, shutdown, err := ServeHTTPWithShutdown(ctx, "test", "1", Options{}, listen)
			if shutdown != nil {
				defer shutdown()
			}
			if err == nil || err.Error() != nonLoopbackBindError {
				t.Fatalf("error = %v, want %q", err, nonLoopbackBindError)
			}
			if addr != "" || shutdown != nil {
				t.Fatalf("rejected bind returned address %q or shutdown function", addr)
			}
		})
	}
}

func TestServeHTTPContextShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := ServeHTTP(ctx, "test", "1", Options{}, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return
		}
		conn.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("listener remains open six seconds after cancellation")
}
