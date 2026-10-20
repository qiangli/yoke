// userland_session.go — the `bashy dks session-server`, `attach` and `logs`
// verbs (contract §4): the executor-side session server plus the clients
// that reach it through the Outpost peer channel, never through kubectl.
//
// Transport: the executor's vknode publishes every pod container port that
// sets hostPort as a transient Outpost app (outpost's TransientAppName rule,
// reimplemented here as transientAppName), reachable through the matrix
// tunnel at <plane base>/matrix/h/<host>/app/<app>/attach. The client finds
// the pod by label, builds that URL, and speaks the attach wire protocol
// (pkg/dagrun/attach) over it.
//
// Failure semantics (contract §4.3): a client disconnect detaches (the
// session keeps running holderless); executor loss is lost (surviving bytes
// replay, then a dedicated exit code — never a re-run); an unreachable
// plane fails closed having run nothing, never locally.
package dks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/attach"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
)

// Terminal attach outcomes share the process exit code with the session, but
// executor loss and takeover have no session code of their own: they map to
// dedicated codes with distinct messages, so `lost` is never confused with a
// child that happened to exit 3 or 4.
const (
	sessionLostExitCode    = 3
	sessionEvictedExitCode = 4
)

// sessionHostLabel is the executor-host label render selects on (the vknode
// host of the pod). The pod's nodeName is the fallback when the label is
// absent.
const sessionHostLabel = "outpost.dhnt.io/host"

// labelValueRE guards label selectors: an id that is not a label value is
// refused before it can widen a `-l` selector. Same shape as the cluster
// package's selector guard.
var labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

func validLabelValue(id string) bool { return labelValueRE.MatchString(id) }

// transientAppName reimplements outpost's TransientAppName
// (outpost/internal/agent/vknode/transient.go): "vk-" + namespace + "-" +
// name + "-" + the first 8 uid characters ("noid" when uid is empty). A
// single-port session pod is addressed by this bare name: outpost tacks
// "-<port>" only onto the non-first ports of multi-port pods, and session
// pods expose exactly one port.
func transientAppName(namespace, name, uid string) string {
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	if short == "" {
		short = "noid"
	}
	return "vk-" + namespace + "-" + name + "-" + short
}

// tunnelAttachURL is the matrix-tunnel address of a session server:
// <plane base>/matrix/h/<host>/app/<app>/attach. The attach handler serves
// on any path, so the trailing /attach is a stable convention, not a route.
func tunnelAttachURL(base, host, app string) string {
	return strings.TrimSuffix(base, "/") + "/matrix/h/" + host + "/app/" + app + "/attach"
}

// tunnelWSScheme maps the paired plane base (http/https, as pairing stores
// it) onto the ws/wss scheme the attach dial needs. A base that already
// speaks ws passes through.
func tunnelWSScheme(base string) (string, error) {
	switch {
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://"), nil
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://"), nil
	case strings.HasPrefix(base, "ws://") || strings.HasPrefix(base, "wss://"):
		return base, nil
	default:
		return "", fmt.Errorf("tunnel base %q is not an http(s) URL", base)
	}
}

// sessionPod is the one session pod a verb resolved by label.
type sessionPod struct {
	Namespace  string
	Name       string
	UID        string
	Host       string
	AttachPort int
}

// podList is the slice of `kubectl get pods -o json` the lookup reads.
type podList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			UID         string            `json:"uid"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	} `json:"items"`
}

// lookupSessionPod finds the session pod by its run label. found=false means
// no pod carries the id (the caller decides batch vs missing); any other
// failure is the plane call itself.
func lookupSessionPod(ctx context.Context, r cluster.Runner, kctx, id string) (pod sessionPod, found bool, err error) {
	if !validLabelValue(id) {
		return sessionPod{}, false, fmt.Errorf("session id %q is not a label-safe id", id)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelRun + "=" + id
	argv := []string{"kubectl"}
	if kctx != "" {
		argv = append(argv, "--context", kctx)
	}
	argv = append(argv, "get", "pods", "-l", sel, "-o", "json")
	stdout, stderr, exit, err := r.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return sessionPod{}, false, fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return sessionPod{}, false, fmt.Errorf("kubectl get pods exited %d: %s", exit, strings.TrimSpace(string(stderr)))
	}
	var list podList
	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	if derr := dec.Decode(&list); derr != nil {
		return sessionPod{}, false, fmt.Errorf("parse kubectl get pods output: %w", derr)
	}
	if len(list.Items) == 0 {
		return sessionPod{}, false, nil
	}
	// One session id names one pod; if the plane ever reports more, the
	// first by name wins deterministically.
	best := 0
	for i := range list.Items {
		if list.Items[i].Metadata.Name < list.Items[best].Metadata.Name {
			best = i
		}
	}
	item := list.Items[best]
	host := item.Metadata.Labels[sessionHostLabel]
	if host == "" {
		host = item.Spec.NodeName
	}
	if host == "" {
		return sessionPod{}, false, fmt.Errorf("session pod %s/%s names no executor host (label %s or nodeName) — nothing ran",
			item.Metadata.Namespace, item.Metadata.Name, sessionHostLabel)
	}
	rawPort := item.Metadata.Annotations[dagrun.AnnotAttachPort]
	port, perr := strconv.Atoi(strings.TrimSpace(rawPort))
	if perr != nil || port < 1 || port > 65535 {
		return sessionPod{}, false, fmt.Errorf("session pod %s/%s has no %s annotation (%q): it predates the session server — re-run it to attach",
			item.Metadata.Namespace, item.Metadata.Name, dagrun.AnnotAttachPort, rawPort)
	}
	return sessionPod{
		Namespace:  item.Metadata.Namespace,
		Name:       item.Metadata.Name,
		UID:        item.Metadata.UID,
		Host:       host,
		AttachPort: port,
	}, true, nil
}

// attachStream is the live view the verbs pump: the surface of
// pkg/dagrun/attach's reconnecting client the verbs need, behind an
// interface so tests script drops and takeovers without a socket.
type attachStream interface {
	io.Reader
	Write(p []byte) error
	Resize(cols, rows uint16) error
	Signal(sig string) error
	Wait() (int, bool, error)
	Reconnect(ctx context.Context) error
	Offset() int64
	Close() error
}

// snapshotConn is the minimal websocket a logs snapshot reads: binary
// frames until the offset frame, then done. *websocket.Conn satisfies it.
type snapshotConn interface {
	ReadMessage() (int, []byte, error)
	Close() error
}

// sessionTerminal is raw mode and window size behind an interface, so tests
// never touch a TTY and piped clients skip both quietly.
type sessionTerminal interface {
	MakeRaw() (restore func(), err error)
	Size() (cols, rows int, err error)
}

// ptyHandle is one executor-side child: the dagrun.PTY the Hub forwards
// input to, plus the wait for its exit code.
type ptyHandle interface {
	dagrun.PTY
	Wait() (int, error)
}

// Terminal pump outcomes.
const (
	streamExit    = "exit"
	streamLost    = "lost"
	streamEvicted = "evicted"
)

// streamEnd is how a pumped stream finished: the session exited (with its
// code), the executor was lost, or another client took over.
type streamEnd struct {
	kind string
	code int // the session's exit code; valid only for streamExit
}

// pumpAttachStream copies a stream to out until it ends. A dropped
// transport redials from the last received byte offset and resumes, so no
// byte is duplicated or lost; exit, loss and eviction are terminal and map
// to distinct streamEnd kinds.
func pumpAttachStream(ctx context.Context, s attachStream, out io.Writer) (streamEnd, error) {
	buf := make([]byte, 4096)
	for {
		n, err := s.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return streamEnd{}, werr
			}
		}
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, io.EOF):
			code, lost, werr := s.Wait()
			if werr != nil {
				return streamEnd{}, werr
			}
			if lost {
				return streamEnd{kind: streamLost}, nil
			}
			return streamEnd{kind: streamExit, code: code}, nil
		case errors.Is(err, dagrun.ErrSessionLost):
			return streamEnd{kind: streamLost}, nil
		case errors.Is(err, dagrun.ErrEvicted):
			return streamEnd{kind: streamEvicted}, nil
		case errors.Is(err, attach.ErrClosed):
			if ctx.Err() != nil {
				return streamEnd{}, ctx.Err()
			}
			return streamEnd{}, err
		default:
			// A transport drop, not a verdict: redial from the last
			// offset. A failed redial (or a cancelled context) ends
			// the verb with the dial error, never a silent retry loop
			// pretending to stream.
			if ctx.Err() != nil {
				return streamEnd{}, ctx.Err()
			}
			if rerr := s.Reconnect(ctx); rerr != nil {
				return streamEnd{}, fmt.Errorf("attach: reconnect: %w", rerr)
			}
		}
	}
}

// pumpStdin forwards terminal input to the session until stdin ends or the
// session stops taking it (a viewer rejects the first write and the pump
// stops quietly; the session keeps streaming to stdout).
func pumpStdin(in io.Reader, s attachStream) {
	if in == nil {
		return
	}
	buf := make([]byte, 1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if werr := s.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// snapshotURL is the viewer endpoint a logs snapshot dials: the whole log
// from offset zero, read-only.
func snapshotURL(endpoint, clientID string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client", clientID)
	q.Set("since", "0")
	q.Set("viewer", "1")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// readSnapshotToEnd copies binary frames to out until the server's offset
// frame — the end of the log at attach time — then stops without following.
// Exit and lost frames end it the same way; anything else is ignored.
func readSnapshotToEnd(conn snapshotConn, out io.Writer) error {
	var frame struct {
		Type string `json:"type"`
	}
	for {
		typ, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("logs: snapshot read: %w", err)
		}
		switch typ {
		case websocket.BinaryMessage:
			if len(msg) > 0 {
				if _, werr := out.Write(msg); werr != nil {
					return werr
				}
			}
		case websocket.TextMessage:
			if jerr := json.Unmarshal(msg, &frame); jerr != nil {
				continue
			}
			switch frame.Type {
			case "offset", "exit", "lost", "evicted":
				return nil
			}
		}
	}
}

// gateSink is the Hub's output sink before the Hub exists: the child can
// print between spawn and NewHub, and those bytes must survive. Writes
// buffer until attach, then flush in order; writes after the session closed
// are dropped (the child outlived its record).
type gateSink struct {
	mu  sync.Mutex
	hub *dagrun.Hub
	buf [][]byte
}

func newGateSink() *gateSink { return &gateSink{} }

func (g *gateSink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	cp := append([]byte(nil), p...)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hub == nil {
		g.buf = append(g.buf, cp)
		return len(p), nil
	}
	return g.appendLocked(cp)
}

func (g *gateSink) appendLocked(p []byte) (int, error) {
	for _, q := range g.buf {
		if _, err := g.hub.Append(q); err != nil {
			g.buf = nil
			if closedSession(err) {
				break
			}
			return 0, err
		}
	}
	g.buf = nil
	if _, err := g.hub.Append(p); err != nil {
		if closedSession(err) {
			return len(p), nil
		}
		return 0, err
	}
	return len(p), nil
}

// attach wires a fresh Hub to the sink and flushes what the child printed
// before the Hub existed. Best-effort: the Hub is fresh, so appends fail
// only if the session already closed.
func (g *gateSink) attach(h *dagrun.Hub) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hub = h
	for _, q := range g.buf {
		if _, err := g.hub.Append(q); err != nil {
			break
		}
	}
	g.buf = nil
}

func closedSession(err error) bool {
	return errors.Is(err, dagrun.ErrSessionClosed) || errors.Is(err, dagrun.ErrHubClosed)
}

// sessionEnv is the seam every side effect of the session verbs goes
// through. Production fills it from the process; tests from fakes, so no
// test execs a binary, touches a TTY, or leaves the loopback interface.
type sessionEnv struct {
	*runEnv
	// tunnelBase resolves the paired plane's matrix-tunnel base URL
	// (http/https). Tests point it at an httptest server.
	tunnelBase func(ctxRec dagrun.Context) (string, error)
	// authHeaders are the paired client's auth headers for tunnel dials.
	// This file invents no scheme: production defaults to none and the
	// pairing story installs its own.
	authHeaders func() http.Header
	// dialStream dials one attach endpoint; production is attach.Attach.
	dialStream func(ctx context.Context, endpoint, clientID string, since int64, viewer bool) (attachStream, error)
	// snapshotDial opens the raw websocket a logs snapshot reads; it
	// carries authHeaders.
	snapshotDial func(ctx context.Context, endpoint string, headers http.Header) (snapshotConn, error)
	// newClientID mints this client's attach identity.
	newClientID func() string
	// terminal is raw mode and window size; tests use a stub.
	terminal func() sessionTerminal
	// startPTY spawns argv under a PTY feeding sink; tests use a fake.
	startPTY func(argv []string, sink io.Writer, ctlSock string) (ptyHandle, error)
	// listen binds the session server's port; tests bind loopback.
	listen func(addr string) (net.Listener, error)
	now    func() time.Time
}

func defaultSessionEnv(env *userlandEnv) *sessionEnv {
	return &sessionEnv{
		runEnv:       defaultRunEnv(env),
		tunnelBase:   defaultTunnelBase,
		authHeaders:  func() http.Header { return nil },
		dialStream:   realDialStream,
		snapshotDial: realSnapshotDial,
		newClientID:  defaultClientID,
		terminal:     func() sessionTerminal { return osTerminal{} },
		startPTY:     startAgentPTY,
		listen:       func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		now:          time.Now,
	}
}

// newUserlandSessionCmds builds logs, attach and the hidden session-server
// over the production seams.
func newUserlandSessionCmds(env *userlandEnv) []*cobra.Command {
	return newSessionCmds(defaultSessionEnv(env))
}

func newSessionCmds(env *sessionEnv) []*cobra.Command {
	return []*cobra.Command{
		newSessionLogsCmd(env),
		newSessionAttachCmd(env),
		newSessionServerCmd(env),
	}
}

// sessionTarget is one resolved verb target: the selected context, its
// plane client, and either the session pod or the batch verdict.
type sessionTarget struct {
	ctx    dagrun.Context
	client *cluster.Client
	pod    sessionPod
	batch  bool // the id names a batch Job: no session server, no attach
}

// resolveTarget maps a session-or-run id onto its pod: selection, then the
// pod lookup by run label, then the batch check. A `run:` reference names a
// weave run, never a dks session. Nothing runs on any path here.
func (env *sessionEnv) resolveTarget(cmd *cobra.Command, verb, id string) (sessionTarget, error) {
	fail := func(err error) (sessionTarget, error) {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: %v\n", verb, err)
		return sessionTarget{}, errUserlandHandled
	}
	if strings.HasPrefix(id, "run:") {
		return fail(fmt.Errorf("%q is a weave run reference, not a dks session — dks session ids are bare ULIDs ('bashy dks ps' lists them)", id))
	}
	if strings.TrimSpace(id) == "" || !validLabelValue(id) {
		return fail(fmt.Errorf("session id %q is not a label-safe id ('bashy dks ps' lists them)", id))
	}
	ctxRec, err := env.selected()
	if err != nil {
		return fail(err)
	}
	client, err := env.planeClient(ctxRec)
	if err != nil {
		return fail(err)
	}
	pod, found, err := lookupSessionPod(cmd.Context(), client.Runner, client.Context, id)
	if err != nil {
		return fail(planeError(ctxRec, err))
	}
	if found {
		return sessionTarget{ctx: ctxRec, client: client, pod: pod}, nil
	}
	runs, err := client.List(cmd.Context(), ctxRec.WorkspaceID)
	if err != nil {
		return fail(planeError(ctxRec, err))
	}
	for _, r := range runs {
		if r.RunID == id && r.Kind == "Job" {
			return sessionTarget{ctx: ctxRec, client: client, batch: true}, nil
		}
	}
	return fail(fmt.Errorf("no session pod for %q in workspace %s ('bashy dks ps' lists runs) — nothing ran", id, workspaceDisplay(ctxRec)))
}

// attachEndpoint builds the tunnel URL of a session pod.
func (env *sessionEnv) attachEndpoint(tgt sessionTarget) (string, error) {
	base, err := env.tunnelBase(tgt.ctx)
	if err != nil {
		return "", err
	}
	wsBase, err := tunnelWSScheme(strings.TrimSpace(base))
	if err != nil {
		return "", err
	}
	return tunnelAttachURL(wsBase, tgt.pod.Host, transientAppName(tgt.pod.Namespace, tgt.pod.Name, tgt.pod.UID)), nil
}

// mapStreamEnd turns a pump outcome into the verb's exit: the session's own
// code on exit (quiet on 0), the dedicated codes with distinct messages on
// loss and takeover.
func mapStreamEnd(cmd *cobra.Command, verb, id string, n int64, end streamEnd) error {
	switch end.kind {
	case streamExit:
		if end.code != 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s exited with code %d\n", verb, id, end.code)
			return &exitStatusError{code: end.code}
		}
		return nil
	case streamLost:
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s is lost (executor gone) — replayed %d bytes; the session never re-runs, start a new one\n",
			verb, id, n)
		return &exitStatusError{code: sessionLostExitCode}
	case streamEvicted:
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s: another client took over (evicted) — attach again to take it back\n",
			verb, id)
		return &exitStatusError{code: sessionEvictedExitCode}
	default:
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s ended without a verdict\n", verb, id)
		return errUserlandHandled
	}
}

func newSessionAttachCmd(env *sessionEnv) *cobra.Command {
	var viewer bool
	cmd := &cobra.Command{
		Use:   "attach <session-or-run-id> [--viewer]",
		Short: "Reattach a session from any paired client",
		Long: `attach reattaches a session's terminal from any paired client:

  bashy dks attach 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8
  bashy dks attach --viewer 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8

The full log replays from the start, then the live session streams. Without
--viewer this client takes the session over; a viewer only watches. A dropped
connection redials from the last byte. The verb exits with the session's exit
code, or a dedicated code when the executor is lost or another client takes
over. A batch run has no terminal: use logs for its output.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runAttach(cmd, args[0], viewer)
		},
	}
	cmd.Flags().BoolVar(&viewer, "viewer", false, "watch read-only: never take the session over")
	return cmd
}

// runAttach dials the session's tunnel URL and pumps it to the terminal:
// stdin forward, raw mode, best-effort window size, redial on drop.
func (env *sessionEnv) runAttach(cmd *cobra.Command, id string, viewer bool) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks attach: %v\n", err)
		return errUserlandHandled
	}
	tgt, err := env.resolveTarget(cmd, "attach", id)
	if err != nil {
		return err
	}
	if tgt.batch {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks attach: run %s is a batch run with no session server — 'bashy dks logs %s' shows its output\n", id, id)
		return errUserlandHandled
	}
	endpoint, err := env.attachEndpoint(tgt)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stream, err := env.dialStream(ctx, endpoint, env.newClientID(), 0, viewer)
	if err != nil {
		return fail(planeError(tgt.ctx, err))
	}
	defer stream.Close()
	go func() {
		<-ctx.Done()
		stream.Close()
	}()

	term := env.terminal()
	restore, err := term.MakeRaw()
	if err != nil {
		return fail(fmt.Errorf("terminal raw mode: %w", err))
	}
	defer restore()
	if cols, rows, serr := term.Size(); serr == nil && cols > 0 && cols < 65536 && rows > 0 && rows < 65536 {
		_ = stream.Resize(uint16(cols), uint16(rows))
	}
	go pumpStdin(cmd.InOrStdin(), stream)

	end, err := pumpAttachStream(ctx, stream, cmd.OutOrStdout())
	if err != nil {
		return fail(err)
	}
	return mapStreamEnd(cmd, "attach", id, stream.Offset(), end)
}

func newSessionLogsCmd(env *sessionEnv) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <run-or-session-id> [-f]",
		Short: "Show a run's or session's output",
		Long: `logs prints a run's or session's output through the peer channel:

  bashy dks logs 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8      print to the current end
  bashy dks logs -f 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8   follow until exit or loss

Without -f the verb prints what arrived and exits 0. With -f it follows to
the recorded exit (exiting with its code) or to executor loss. Batch runs
have no session server: their output arrives with the RunRecord, and logs
says so instead of streaming.`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runLogs(cmd, args[0], follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow the session until it exits or is lost")
	return cmd
}

// snapshotTimeout bounds a logs snapshot: the offset frame arrives right
// after the replay, so a healthy tunnel answers far inside it.
const snapshotTimeout = 30 * time.Second

func (env *sessionEnv) runLogs(cmd *cobra.Command, id string, follow bool) error {
	tgt, err := env.resolveTarget(cmd, "logs", id)
	if err != nil {
		return err
	}
	if tgt.batch {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks logs: logs for batch runs arrive with the RunRecord path — run %s is a batch run with no session server; describe its RunRecord instead of streaming\n", id)
		return errUserlandHandled
	}
	endpoint, err := env.attachEndpoint(tgt)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks logs: %v\n", err)
		return errUserlandHandled
	}
	if !follow {
		return env.snapshotLogs(cmd, tgt, endpoint, id)
	}
	return env.followLogs(cmd, tgt, endpoint, id)
}

// snapshotLogs prints the log to its current end: a read-only dial that
// stops at the offset frame and exits 0.
func (env *sessionEnv) snapshotLogs(cmd *cobra.Command, tgt sessionTarget, endpoint, id string) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks logs: %v\n", err)
		return errUserlandHandled
	}
	_ = tgt
	raw, err := snapshotURL(endpoint, env.newClientID())
	if err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), snapshotTimeout)
	defer cancel()
	conn, err := env.snapshotDial(ctx, raw, env.authHeaders())
	if err != nil {
		return fail(planeError(sessionTargetCtx(tgt), err))
	}
	defer conn.Close()
	if err := readSnapshotToEnd(conn, cmd.OutOrStdout()); err != nil {
		return fail(err)
	}
	_ = id
	return nil
}

func sessionTargetCtx(tgt sessionTarget) dagrun.Context { return tgt.ctx }

// followLogs streams a session like attach but read-only: until the
// recorded exit (reporting its code) or executor loss.
func (env *sessionEnv) followLogs(cmd *cobra.Command, tgt sessionTarget, endpoint, id string) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks logs: %v\n", err)
		return errUserlandHandled
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stream, err := env.dialStream(ctx, endpoint, env.newClientID(), 0, true)
	if err != nil {
		return fail(planeError(tgt.ctx, err))
	}
	defer stream.Close()
	go func() {
		<-ctx.Done()
		stream.Close()
	}()
	end, err := pumpAttachStream(ctx, stream, cmd.OutOrStdout())
	if err != nil {
		return fail(err)
	}
	return mapStreamEnd(cmd, "logs", id, stream.Offset(), end)
}

// sessionServerOptions are the hidden verb's flags: the session identity,
// its home, and the published attach port. Everything after `--` is the
// session's argv.
type sessionServerOptions struct {
	session string
	home    string
	port    int
}

func newSessionServerCmd(env *sessionEnv) *cobra.Command {
	var o sessionServerOptions
	cmd := &cobra.Command{
		Use:    "session-server --session <id> --home <dir> --port <p> -- <argv...>",
		Short:  "Run the executor-side session server (internal)",
		Long:   `session-server runs one session inside its executor: argv under a PTY, output into the session log, the attach protocol on the flagged port. It is started by the rendered Pod, never by hand.`,
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.runSessionServer(cmd, o, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.Flags().StringVar(&o.session, "session", "", "session id (the run id of the rendered Pod)")
	cmd.Flags().StringVar(&o.home, "home", "", "workspace home holding .dks/sessions/<id>/")
	cmd.Flags().IntVar(&o.port, "port", 0, "attach port to serve (the Pod's published hostPort)")
	return cmd
}

// runSessionServer serves one session to its end: bind the attach port, run
// argv under a PTY into a fresh Hub, serve the attach protocol until the
// child exits, record the exit exactly once, and exit with the child's code.
// A client disconnect detaches (the child keeps running); only the child's
// own exit ends the server.
func (env *sessionEnv) runSessionServer(cmd *cobra.Command, o sessionServerOptions, argv []string) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks session-server: %v\n", err)
		return errUserlandHandled
	}
	if strings.TrimSpace(o.session) == "" {
		return fail(errors.New("no --session id"))
	}
	if strings.TrimSpace(o.home) == "" {
		return fail(errors.New("no --home dir"))
	}
	if o.port < 1 || o.port > 65535 {
		return fail(fmt.Errorf("--port %d is not a port (1-65535)", o.port))
	}
	if len(argv) == 0 {
		return fail(errors.New("no command after --"))
	}
	dir := filepath.Join(o.home, ".dks", "sessions", o.session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	// The executor-side record keys workspace and run on the session id:
	// the workspace/run scoping the verbs use lives in the pod labels, and
	// interactive and long-running sessions are both PTY sessions here. A
	// follow-up may carry workspace and kind explicitly.
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "executor"
	}
	sess, err := dagrun.NewSession(o.session, o.session, o.session,
		dagrun.KindInteractive, host, runtime.GOOS, runtime.GOARCH, "", env.now())
	if err != nil {
		return fail(err)
	}
	// Bind before spawning: 0.0.0.0 so the Pod's hostPort publish reaches
	// the server, which vknode publishes on as the transient app.
	ln, err := env.listen(fmt.Sprintf("0.0.0.0:%d", o.port))
	if err != nil {
		return fail(fmt.Errorf("listen: %w", err))
	}
	defer ln.Close()
	sink := newGateSink()
	handle, err := env.startPTY(argv, sink, filepath.Join(dir, "ctl.sock"))
	if err != nil {
		return fail(fmt.Errorf("start session: %w", err))
	}
	hub, err := dagrun.NewHub(dir, sess, handle)
	if err != nil {
		return fail(err)
	}
	sink.attach(hub)
	fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks session-server: session %s serving attach on %s\n", o.session, ln.Addr())
	go func() {
		_ = http.Serve(ln, attach.Handler(hub))
	}()
	code, werr := handle.Wait()
	if werr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks session-server: session child: %v\n", werr)
	}
	if rerr := hub.RecordExit(code, env.now()); rerr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks session-server: record exit: %v\n", rerr)
	}
	if code != 0 {
		return &exitStatusError{code: code}
	}
	return nil
}

// realDialStream is the production attach dial: pkg/dagrun/attach's
// reconnecting client. The paired client's auth rides whatever dialer the
// pairing story installs in this seam; this file sends no credentials.
func realDialStream(ctx context.Context, endpoint, clientID string, since int64, viewer bool) (attachStream, error) {
	var opts []attach.Option
	if viewer {
		opts = append(opts, attach.Viewer())
	}
	return attach.Attach(ctx, endpoint, clientID, since, opts...)
}

// realSnapshotDial opens the snapshot websocket carrying the paired
// client's headers.
func realSnapshotDial(ctx context.Context, endpoint string, headers http.Header) (snapshotConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	return ws, nil
}

// tunnelBaseEnv names the paired plane's matrix-tunnel base URL.
const tunnelBaseEnv = "BASHY_DKS_TUNNEL_BASE"

// defaultTunnelBase reads the paired plane base from the environment. The
// pairing story (Sprint 482 satellite) owns the value; this verb only reads
// it, never guesses it.
func defaultTunnelBase(_ dagrun.Context) (string, error) {
	base := strings.TrimSuffix(strings.TrimSpace(os.Getenv(tunnelBaseEnv)), "/")
	if base == "" {
		return "", fmt.Errorf("no matrix tunnel base: set $%s to the paired plane base — nothing ran", tunnelBaseEnv)
	}
	return base, nil
}

// defaultClientID identifies this client's attachments: host plus pid, so
// two clients on one host never share a holder.
func defaultClientID() string {
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		return fmt.Sprintf("client-%s-%d", strings.TrimSpace(host), os.Getpid())
	}
	return fmt.Sprintf("client-pid-%d", os.Getpid())
}

// osTerminal is the production sessionTerminal: raw mode and size only when
// stdin/stdout are terminals; piped clients skip both and stream plainly.
type osTerminal struct{}

func (osTerminal) MakeRaw() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, nil
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}, err
	}
	return func() { _ = term.Restore(fd, old) }, nil
}

func (osTerminal) Size() (int, int, error) {
	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0, err
	}
	return cols, rows, nil
}

// agentPTYHandle runs argv under agentpty: PTY allocation, trust-prompt
// clearing over the control socket, and output capture are agentpty's;
// input arrives from attached clients through the control socket's verbatim
// frames.
type agentPTYHandle struct {
	cmd     *exec.Cmd
	ctlSock string
	done    chan struct{}
	mu      sync.Mutex
	code    int
	runErr  error
}

// startAgentPTY spawns argv under a PTY whose output feeds sink. It returns
// once the child is spawned; Wait reports its exit.
func startAgentPTY(argv []string, sink io.Writer, ctlSock string) (ptyHandle, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to run under the PTY")
	}
	h := &agentPTYHandle{
		cmd:     exec.Command(argv[0], argv[1:]...),
		ctlSock: ctlSock,
		done:    make(chan struct{}),
	}
	h.cmd.Env = os.Environ()
	go func() {
		defer close(h.done)
		code, _, runErr := agentpty.Run(h.cmd, sink, agentpty.Options{CtlSock: ctlSock, Capture: true})
		h.mu.Lock()
		defer h.mu.Unlock()
		h.code, h.runErr = code, runErr
	}()
	return h, nil
}

// WriteInput delivers client keystrokes to the session's stdin through the
// control socket's verbatim frames: byte-exact, never line-submitted.
func (h *agentPTYHandle) WriteInput(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := agentpty.SendFrame(h.ctlSock, agentpty.VerbatimFrame(p)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize has no agentpty route in this slice: the PTY keeps the geometry it
// started with (the executor's fallback), and the client learns that from
// the error frame instead of a silent wrong size.
func (h *agentPTYHandle) Resize(_, _ uint16) error {
	return errors.New("session resize is not wired to the executor PTY in this slice")
}

// Signal delivers INT, TERM, HUP, QUIT or KILL to the session's child. The
// child is a session leader, so a shell forwards job-control signals to its
// own foreground group; anything else is refused loudly.
func (h *agentPTYHandle) Signal(sig string) error {
	if h.cmd.Process == nil {
		return errors.New("session has no process to signal")
	}
	var s os.Signal
	switch strings.ToUpper(strings.TrimSpace(sig)) {
	case "INT":
		s = os.Interrupt
	case "TERM":
		s = syscall.SIGTERM
	case "HUP":
		s = syscall.SIGHUP
	case "QUIT":
		s = syscall.SIGQUIT
	case "KILL":
		s = syscall.SIGKILL
	default:
		return fmt.Errorf("unknown signal %q (want INT, TERM, HUP, QUIT or KILL)", sig)
	}
	return h.cmd.Process.Signal(s)
}

// Wait blocks until the session's child exits and reports its code.
func (h *agentPTYHandle) Wait() (int, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.code, h.runErr
}
