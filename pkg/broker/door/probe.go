package door

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// StatusSchemaVersion is the envelope `bashy llm status --json` emits.
const StatusSchemaVersion = "bashy-llm-status-v1"

// ProbeTimeout bounds a whole probe, so a down door never hangs a caller.
const ProbeTimeout = 2 * time.Second

// Backend is one of the door's backends and its health.
type Backend struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// Status is what a running (or absent) door reports about itself.
type Status struct {
	SchemaVersion string    `json:"schema_version"`
	Up            bool      `json:"up"`
	PID           int       `json:"pid,omitempty"`
	Version       string    `json:"version,omitempty"`
	Listen        string    `json:"listen"`
	Socket        string    `json:"socket,omitempty"`
	Backends      []Backend `json:"backends"`
	LocalModels   int       `json:"local_models"`
	AgentEntries  int       `json:"agent_entries"`
	MaxInFlight   int       `json:"max_in_flight,omitempty"`
	QueueDepth    int       `json:"queue_depth"`
	Error         string    `json:"error,omitempty"`
}

// Probe asks the door at BaseURL for its status. A door that is not running
// is not a Go error: it returns Status{Up:false, Error:...} and a nil error.
// A non-nil error means the probe itself could not be made.
func Probe(ctx context.Context) (Status, error) {
	st := Status{SchemaVersion: StatusSchemaVersion, Listen: strings.TrimPrefix(BaseURL(), "http://"), Backends: []Backend{}}
	if sock, err := SocketPath(); err == nil {
		st.Socket = sock
	}
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	token, err := Token()
	if err != nil {
		return st, fmt.Errorf("door: read token: %w", err)
	}
	var health struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
		Local   bool   `json:"local_engine"`
		Engine  bool   `json:"engine_up"`
		Device  struct {
			Queued map[string]int `json:"queued"`
		} `json:"device"`
		CLI json.RawMessage `json:"cligw"`
	}
	if err := get(ctx, "/health", token, &health); err != nil {
		st.Error = downMessage(err)
		var he *httpErr
		if errors.As(err, &he) {
			st.Up = he.status == http.StatusUnauthorized
			if !st.Up {
				return st, nil
			}
			st.Error = "door answers but rejected the owner token (HTTP 401)"
		}
		return st, nil
	}
	st.Up, st.PID, st.Version = true, health.PID, health.Version
	for _, n := range health.Device.Queued {
		st.QueueDepth += n
	}
	st.Backends = append(st.Backends, Backend{Name: "ollama-local", Present: health.Local, Healthy: health.Local && health.Engine})
	cli := Backend{Name: "cligw"}
	if len(health.CLI) > 0 {
		var c struct {
			Status string `json:"status"`
			Policy struct {
				MaxInFlight int `json:"max_in_flight"`
			} `json:"policy"`
		}
		if json.Unmarshal(health.CLI, &c) == nil {
			cli.Present, cli.Healthy, cli.Detail = true, c.Status == "ok", c.Status
			st.MaxInFlight = c.Policy.MaxInFlight
		}
	}
	st.Backends = append(st.Backends, cli)

	var models struct {
		Data []struct {
			Kind string `json:"x_kind"`
			Loc  string `json:"x_locality"`
		} `json:"data"`
	}
	if err := get(ctx, "/v1/models", token, &models); err != nil {
		st.Error = "door is up but /v1/models failed: " + err.Error()
		return st, nil
	}
	for _, m := range models.Data {
		if m.Kind == "local" || m.Loc == "local" {
			st.LocalModels++
		} else {
			st.AgentEntries++
		}
	}
	return st, nil
}

type httpErr struct {
	status int
	msg    string
}

func (e *httpErr) Error() string { return e.msg }

func get(ctx context.Context, path, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: ProbeTimeout}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &httpErr{resp.StatusCode, fmt.Sprintf("GET %s: HTTP %d", path, resp.StatusCode)}
	}
	return json.Unmarshal(data, out)
}

func downMessage(err error) string {
	var he *httpErr
	switch {
	case errors.As(err, &he):
		return he.msg
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("no answer from the model door at %s within %s", BaseURL(), ProbeTimeout)
	}
	var ne net.Error
	if errors.As(err, &ne) || strings.Contains(err.Error(), "refused") {
		return fmt.Sprintf("connection refused: no model door listening on %s (start it: bashy llm up)", BaseURL())
	}
	return fmt.Sprintf("model door unreachable at %s: %v", BaseURL(), err)
}
