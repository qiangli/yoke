package cligw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/spf13/cobra"
)

const startSessionDoorHint = "start the model door with `bashy llm up` (use `bashy llm up --session` for subscriptions that require your login session)"

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "session", Short: "manage scoped model access for a run"}
	cmd.AddCommand(newSessionOpenCmd(), newSessionCloseCmd(), newSessionListCmd(), newSessionForwardCmd())
	return cmd
}

func newSessionOpenCmd() *cobra.Command {
	var opts runSessionOptions
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "open",
		Short: "open a scoped Unix socket and issue its run token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := json.Marshal(opts)
			if err != nil {
				return err
			}
			var info runSessionInfo
			found, err := visitSessionDoors(cmd.Context(), http.MethodPost, nil, body, func(response io.Reader) error {
				return json.NewDecoder(response).Decode(&info)
			})
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("model door is unavailable; %s", startSessionDoorHint)
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Label: %s\nToken: %s\nSocket: %s\nExpires: %s\nModels: %s\n",
				info.Label, info.Token, info.Socket, info.ExpiresAt.Format(time.RFC3339), strings.Join(info.Models, ","))
			return err
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&opts.Models, "model", nil, "allowed bindings (comma-separated)")
	f.StringVar(&opts.Band, "band", "", "allowed band (L3, L4 or L5)")
	f.DurationVar(&opts.TTL, "ttl", 30*time.Minute, "token lifetime")
	f.IntVar(&opts.MaxRequests, "max-requests", 200, "maximum requests accepted on the session socket")
	f.StringVar(&opts.Label, "label", "", "label identifying this run")
	f.BoolVar(&asJSON, "json", false, "print the session as JSON")
	cmd.MarkFlagsMutuallyExclusive("model", "band")
	cmd.MarkFlagsOneRequired("model", "band")
	_ = cmd.MarkFlagRequired("label")
	return cmd
}

func newSessionCloseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "close LABEL",
		Short: "revoke a run token and remove its socket",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := visitSessionDoors(cmd.Context(), http.MethodDelete, url.Values{"label": {args[0]}}, nil, func(io.Reader) error { return nil })
			return err
		},
	}
}

func newSessionListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list run sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sessions := []runSessionInfo{}
			_, err := visitSessionDoors(cmd.Context(), http.MethodGet, nil, nil, func(response io.Reader) error {
				var rows []runSessionInfo
				if err := json.NewDecoder(response).Decode(&rows); err != nil {
					return err
				}
				sessions = append(sessions, rows...)
				return nil
			})
			if err != nil {
				return err
			}
			sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Label < sessions[j].Label })
			out := cmd.OutOrStdout()
			if asJSON {
				return json.NewEncoder(out).Encode(sessions)
			}
			for _, info := range sessions {
				if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", info.Label, info.Socket, info.ExpiresAt.Format(time.RFC3339), strings.Join(info.Models, ",")); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print sessions as JSON")
	return cmd
}

// The login-session door can reach subscription logins held in the user's
// session. Prefer it for new runs. List and close cover both door lifetimes
// so a run can still be closed if a login door starts after it was opened.
func visitSessionDoors(ctx context.Context, method string, query url.Values, body []byte, receive func(io.Reader) error) (bool, error) {
	host, err := StateDir()
	if err != nil {
		return false, err
	}
	login, err := door.SessionHome()
	if err != nil {
		return false, err
	}
	var found bool
	for _, dir := range []string{filepath.Join(login, "cligw"), host} {
		ok, err := requestSessionDoor(ctx, dir, method, query, body, receive)
		if err != nil {
			return found, err
		}
		if ok && method == http.MethodPost {
			return true, nil
		}
		found = found || ok
	}
	return found, nil
}

func requestSessionDoor(ctx context.Context, dir, method string, query url.Values, body []byte, receive func(io.Reader) error) (bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, "endpoint.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var endpoint Endpoint
	if err := json.Unmarshal(data, &endpoint); err != nil {
		return false, fmt.Errorf("read model door endpoint: %w", err)
	}
	base, err := url.Parse(endpoint.BaseURL)
	if err != nil || base.Scheme != "http" || base.Host == "" || base.User != nil {
		return false, fmt.Errorf("invalid local model door endpoint; %s", startSessionDoorHint)
	}
	if endpoint.Socket == "" {
		base.Host = net.JoinHostPort("127.0.0.1", base.Port())
	}
	target := base.JoinPath(sessionPath)
	target.RawQuery = query.Encode()
	target.Fragment = ""

	token, err := os.ReadFile(filepath.Join(dir, "token"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	client := endpoint.Client()
	if endpoint.Socket == "" {
		client.Transport = &http.Transport{}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reach model door: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var refusal struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&refusal)
		if refusal.Error != "" {
			return false, errors.New(refusal.Error)
		}
		return false, fmt.Errorf("model door returned %s for session management; %s", resp.Status, startSessionDoorHint)
	}
	return true, receive(resp.Body)
}

func newSessionForwardCmd() *cobra.Command {
	var socket, listen string
	cmd := &cobra.Command{
		Use:   "forward",
		Short: "forward loopback HTTP to a run's Unix socket",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return forwardSession(ctx, cmd.OutOrStdout(), socket, listen)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "absolute Unix socket path")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "loopback IP and local port")
	_ = cmd.MarkFlagRequired("socket")
	return cmd
}

func forwardSession(ctx context.Context, out io.Writer, socket, listen string) error {
	if !filepath.IsAbs(socket) {
		return fmt.Errorf("--socket must be an absolute Unix socket path")
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("--listen must use a loopback IP and port (for example 127.0.0.1:0)")
	}
	fi, err := os.Stat(socket)
	if err != nil {
		return fmt.Errorf("access session socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("--socket must name a Unix socket")
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()

	transport := unixTransport(socket)
	defer transport.CloseIdleConnections()
	target := &url.URL{Scheme: "http", Host: "localhost"}
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite:   func(r *httputil.ProxyRequest) { r.SetURL(target) },
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 30 * time.Second}
	if _, err := fmt.Fprintf(out, "http://%s/v1\n", listener.Addr()); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
