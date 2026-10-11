package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// The owner side of the monitor's control socket. A process that hosts
// internal capabilities (outpost, meet) asks `bashy supervisord` which of its
// units are wanted now (`desired`), applies them with Internal, and tells the
// monitor what happened (`report`). The monitor owns policy; the owner owns
// the capability. Wire shape (one JSON request and response per connection):
//
//	{"op":"desired","owner":NAME}  -> {"ok":true,"units":[Status…]}  units[i].enabled = apply now
//	{"op":"report","status":{…}}   -> {"ok":true}
//
// The wire types are defined by bashy/internal/agentos/supervisord_ipc.go.

// ErrNoMonitor: nothing answers on the control socket.
var ErrNoMonitor = errors.New("bashy supervisord is not running")

// SupervisordSocket is the monitor's control socket path:
// $BASHY_SUPERVISORD_SOCKET, else $BASHY_HOME/service/supervisord.sock
// (default ~/.bashy/service/supervisord.sock).
func SupervisordSocket() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_SUPERVISORD_SOCKET")); p != "" {
		return p
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "service", "supervisord.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".bashy", "service", "supervisord.sock")
}

type ownerRequest struct {
	Op     string `json:"op"`
	Owner  string `json:"owner,omitempty"`
	Status Status `json:"status,omitzero"`
}

type ownerResponse struct {
	OK    bool     `json:"ok"`
	Error string   `json:"error,omitempty"`
	Units []Status `json:"units,omitempty"`
}

// Client speaks the owner half of the control protocol.
type Client struct {
	Socket  string        // default SupervisordSocket()
	Timeout time.Duration // per call; default 5s
}

func (c Client) call(req ownerRequest) (ownerResponse, error) {
	path, timeout := c.Socket, c.Timeout
	if path == "" {
		path = SupervisordSocket()
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return ownerResponse{}, fmt.Errorf("%w (%s): %v", ErrNoMonitor, path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ownerResponse{}, err
	}
	var resp ownerResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return ownerResponse{}, err
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = "monitor refused " + req.Op
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// Desired lists the internal units owner hosts; Status.Enabled means the
// monitor wants the unit applied now.
func (c Client) Desired(owner string) ([]Status, error) {
	resp, err := c.call(ownerRequest{Op: "desired", Owner: owner})
	return resp.Units, err
}

// Report tells the monitor the owner's view of one of its units.
func (c Client) Report(st Status) error {
	_, err := c.call(ownerRequest{Op: "report", Status: st})
	return err
}

// Owner keeps one process's internal units in the state the monitor wants.
// It is generic: it knows capability ids only through the declarations the
// monitor names, never a unit by name.
type Owner struct {
	Name     string    // the owner name declarations carry (owner: meet)
	Registry *Registry // the capabilities this process hosts
	Client   Client
	// Resolve returns the declaration of a unit this owner was told about.
	// Default: the fleet catalog.
	Resolve func(unit string) (fleet.Service, error)
	// Every is the poll interval; default 2s.
	Every time.Duration
	// Log receives one line per failure change; nil discards.
	Log io.Writer

	internal *Internal
	mu       sync.Mutex
	applied  map[string]fleet.Service
	lastErr  string
}

func (o *Owner) provider() *Internal {
	if o.internal == nil {
		o.internal = NewInternal(o.Name, o.Registry)
		o.applied = map[string]fleet.Service{}
	}
	return o.internal
}

func (o *Owner) resolve(unit string) (fleet.Service, error) {
	if o.Resolve != nil {
		return o.Resolve(unit)
	}
	s, ok := fleet.New().Service(unit)
	if !ok {
		return fleet.Service{}, fmt.Errorf("service: unit %q is not declared", unit)
	}
	return s, nil
}

// Sync is one pass: fetch the desired set, apply each unit, report the
// result. The reported state is what the owner observes, so a monitor that
// restarts is re-informed on the next pass without any special case.
func (o *Owner) Sync(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.provider()
	want, err := o.Client.Desired(o.Name)
	if err != nil {
		return err
	}
	var errs []error
	for _, st := range want {
		if err := o.syncUnit(ctx, in, st); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", st.Unit, err))
		}
	}
	return errors.Join(errs...)
}

func (o *Owner) syncUnit(ctx context.Context, in *Internal, want Status) error {
	decl, err := o.resolve(want.Unit)
	if err != nil {
		return err
	}
	rt := Runtime{Env: decl.Env}
	cur := in.Status(want.Unit).State
	var applyErr error
	switch {
	case want.Enabled && (cur == Inactive || cur == Failed):
		applyErr = in.Activate(ctx, decl, rt)
	case want.Enabled:
		if prev, ok := o.applied[want.Unit]; !ok || !reflect.DeepEqual(prev, decl) {
			applyErr = in.Configure(ctx, decl, rt)
			if errors.Is(applyErr, ErrRestartRequired) {
				if applyErr = in.Deactivate(ctx, want.Unit); applyErr == nil {
					applyErr = in.Activate(ctx, decl, rt)
				}
			}
		}
	default:
		applyErr = in.Settle(ctx, decl)
	}
	if applyErr == nil {
		if want.Enabled {
			o.applied[want.Unit] = decl
		} else {
			delete(o.applied, want.Unit)
		}
	}
	st := in.Status(want.Unit)
	st.Enabled = want.Enabled
	if applyErr != nil {
		st.State, st.Reason = Failed, applyErr.Error()
	} else if st.State == Active {
		if herr := in.Health(ctx, want.Unit); herr != nil {
			st.State, st.Reason, st.Health = Degraded, herr.Error(), herr.Error()
		} else {
			st.Health = "ok"
		}
	}
	if err := o.Client.Report(st); err != nil {
		return err
	}
	return applyErr
}

// Run syncs immediately and then every interval until ctx ends, when it
// deactivates everything it applied. A missing monitor is not an error: the
// process then runs unmanaged, and the loop keeps looking for one.
func (o *Owner) Run(ctx context.Context) {
	every := o.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		o.note(o.Sync(ctx))
		select {
		case <-ctx.Done():
			o.Shutdown()
			return
		case <-t.C:
		}
	}
}

// note logs a failure only when it changes.
func (o *Owner) note(err error) {
	msg := ""
	if err != nil && !errors.Is(err, ErrNoMonitor) {
		msg = err.Error()
	}
	if msg == o.lastErr {
		return
	}
	o.lastErr = msg
	if msg != "" && o.Log != nil {
		fmt.Fprintf(o.Log, "%s: service owner: %s\n", o.Name, msg)
	}
}

// Shutdown deactivates every unit this owner applied; the monitor reads the
// owner's disappearance as the units going inactive.
func (o *Owner) Shutdown() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.internal == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for unit := range o.applied {
		_ = o.internal.Deactivate(ctx, unit)
		st := o.internal.Status(unit)
		_ = o.Client.Report(st)
	}
	o.applied = map[string]fleet.Service{}
}
