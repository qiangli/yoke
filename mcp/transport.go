package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServeStdioWithOptions serves stdio with the supplied server options, using
// the same end-of-session semantics as ServeStdio.
func ServeStdioWithOptions(ctx context.Context, name, version string, opts Options) error {
	err := NewServerWithOptions(name, version, opts).Run(ctx, &mcpsdk.StdioTransport{})
	if isCleanShutdown(err) {
		return nil
	}
	return err
}

const nonLoopbackBindError = "refusing non-loopback bind without an authorization issuer, resource URI and audience (post-1.0)"

// ServeHTTP starts a loopback-only, stateless Streamable HTTP server at /mcp.
// An empty listen address selects 127.0.0.1:0. It returns the bound address
// without printing; cancelling ctx shuts the server down with a five-second
// grace period. Use ServeHTTPWithShutdown to wait for shutdown and its errors.
func ServeHTTP(ctx context.Context, name, version string, opts Options, listen string) (addr string, err error) {
	addr, _, err = ServeHTTPWithShutdown(ctx, name, version, opts, listen)
	return addr, err
}

// ServeHTTPWithShutdown is ServeHTTP with an idempotent shutdown function.
// Shutdown cancels serving and waits for graceful shutdown (at most five
// seconds before active connections are closed). It reports serving or
// shutdown errors; normal cancellation returns nil.
func ServeHTTPWithShutdown(ctx context.Context, name, version string, opts Options, listen string) (addr string, shutdown func() error, err error) {
	listen, err = loopbackListenAddress(ctx, listen)
	if err != nil {
		return "", nil, err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", listen)
	if err != nil {
		return "", nil, err
	}
	server := NewServerWithOptions(name, version, opts)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return server }, &mcpsdk.StreamableHTTPOptions{Stateless: true}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serving := make(chan error, 1)
	go func() { serving <- srv.Serve(listener) }()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var result error
	go func() {
		defer close(done)
		defer cancel()
		select {
		case result = <-serving:
			_ = srv.Close()
		case <-ctx.Done():
			grace, stop := context.WithTimeout(context.Background(), 5*time.Second)
			result = srv.Shutdown(grace)
			stop()
			if result != nil {
				_ = srv.Close()
			}
			serveErr := <-serving
			if result == nil {
				result = serveErr
			}
		}
		if errors.Is(result, http.ErrServerClosed) {
			result = nil
		}
	}()
	return listener.Addr().String(), func() error { cancel(); <-done; return result }, nil
}

// Resolve once, validate every answer, and bind a numeric address so a second
// DNS lookup cannot change the interface on which the server listens.
func loopbackListenAddress(ctx context.Context, listen string) (string, error) {
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if host == "localhost" {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	refuse := errors.New(nonLoopbackBindError)
	if host == "" {
		return "", refuse
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return "", refuse
	}
	for _, address := range addresses {
		if !address.IP.IsLoopback() || address.Zone != "" {
			return "", refuse
		}
	}
	return net.JoinHostPort(addresses[0].IP.String(), port), nil
}
