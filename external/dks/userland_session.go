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
	"gopkg.in/yaml.v3"

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

// sessionHostLabel is the Outpost host label. The vknode sets it on its
// NODE (the node name itself is "<host>-vk-<provider>", not the host), so
// the executor host is read from the pod's node; a pod that carries the
// label wins (forward compatible). The node name is never a fallback: the
// tunnel routes by Outpost host, and a plain kubelet has none.
const sessionHostLabel = "outpost.dhnt.io/host"

// labelValueRE guards label selectors: an id that is not a label value is
// refused before it can widen a `-l` selector. Same shape as the cluster
// package's selector guard.
var labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

func validLabelValue(id string) bool { return labelValueRE.MatchString(id) }

// nodeNameRE is a DNS-1123 subdomain: the shape of a Kubernetes node name,
// checked before it becomes a kubectl operand.
var nodeNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

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
	// Phase is the pod's session phase (cluster.SessionPhase); Terminated
	// its container's terminated state, nil while it runs.
	Phase      cluster.Phase
	Terminated *cluster.ContainerTerminated
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
		Status struct {
			Phase             string                    `json:"phase"`
			Reason            string                    `json:"reason"`
			ContainerStatuses []cluster.ContainerStatus `json:"containerStatuses"`
		} `json:"status"`
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
		if host, err = nodeOutpostHost(ctx, r, kctx, item.Metadata.Namespace, item.Metadata.Name, item.Spec.NodeName); err != nil {
			return sessionPod{}, false, err
		}
	}
	if !validLabelValue(host) {
		return sessionPod{}, false, fmt.Errorf("session pod %s/%s names executor host %q, which is not a valid Outpost host — nothing ran",
			item.Metadata.Namespace, item.Metadata.Name, host)
	}
	rawPort := item.Metadata.Annotations[dagrun.AnnotAttachPort]
	port, perr := strconv.Atoi(strings.TrimSpace(rawPort))
	if perr != nil || port < 1 || port > 65535 {
		return sessionPod{}, false, fmt.Errorf("session pod %s/%s has no %s annotation (%q): it predates the session server — re-run it to attach",
			item.Metadata.Namespace, item.Metadata.Name, dagrun.AnnotAttachPort, rawPort)
	}
	phase, term := cluster.SessionPhase(item.Status.Phase, item.Status.Reason, item.Status.ContainerStatuses)
	return sessionPod{
		Namespace:  item.Metadata.Namespace,
		Name:       item.Metadata.Name,
		UID:        item.Metadata.UID,
		Host:       host,
		AttachPort: port,
		Phase:      phase,
		Terminated: term,
	}, true, nil
}

// nodeObject is the slice of `kubectl get node -o json` the host lookup reads.
type nodeObject struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// nodeOutpostHost resolves the Outpost host of the node a session pod runs
// on from the node's sessionHostLabel. A node without it is a plain kubelet:
// attach rides the Outpost tunnel, so that fails closed rather than dialing
// a route named after the node.
func nodeOutpostHost(ctx context.Context, r cluster.Runner, kctx, ns, pod, node string) (string, error) {
	if node == "" {
		return "", fmt.Errorf("session pod %s/%s is not scheduled on a node yet (no spec.nodeName) — nothing ran; retry once it is running", ns, pod)
	}
	if !nodeNameRE.MatchString(node) {
		return "", fmt.Errorf("session pod %s/%s names node %q, which is not a valid node name — nothing ran", ns, pod, node)
	}
	argv := []string{"kubectl"}
	if kctx != "" {
		argv = append(argv, "--context", kctx)
	}
	argv = append(argv, "get", "node", node, "-o", "json")
	stdout, stderr, exit, err := r.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return "", fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return "", fmt.Errorf("kubectl get node %s exited %d: %s", node, exit, strings.TrimSpace(string(stderr)))
	}
	var obj nodeObject
	if derr := json.Unmarshal(stdout, &obj); derr != nil {
		return "", fmt.Errorf("parse kubectl get node %s output: %w", node, derr)
	}
	host := strings.TrimSpace(obj.Metadata.Labels[sessionHostLabel])
	if host == "" {
		return "", fmt.Errorf("session pod %s/%s runs on node %s, which carries no %s label — attach and logs ride the Outpost tunnel and need an Outpost (vknode) executor host; nothing ran",
			ns, pod, node, sessionHostLabel)
	}
	return host, nil
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
	// authHeaders are the paired client's auth headers for one tunnel
	// endpoint, resolved before every dial so a refreshed cookie reaches a
	// redial. Production is pairedAuthHeaders.
	authHeaders func(endpoint string) (http.Header, error)
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
	// lingerAfter starts the post-exit read-only window: the server keeps
	// serving replay and the exit frame until the returned channel fires.
	// Production is time.After; tests close a channel.
	lingerAfter func(d time.Duration) <-chan time.Time
	now         func() time.Time
}

func defaultSessionEnv(env *userlandEnv) *sessionEnv {
	s := &sessionEnv{
		runEnv:       defaultRunEnv(env),
		tunnelBase:   defaultTunnelBase,
		authHeaders:  pairedAuthHeaders,
		snapshotDial: realSnapshotDial,
		newClientID:  defaultClientID,
		terminal:     func() sessionTerminal { return osTerminal{} },
		startPTY:     startAgentPTY,
		listen:       func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		lingerAfter:  time.After,
		now:          time.Now,
	}
	s.dialStream = func(ctx context.Context, endpoint, clientID string, since int64, viewer bool) (attachStream, error) {
		return realDialStream(ctx, endpoint, clientID, since, viewer, func() (http.Header, error) {
			return s.authHeaders(endpoint)
		})
	}
	return s
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

// terminalDialFailure is the verdict on a failed dial to a session pod that
// already terminated: its server died with its container, so the dial error
// (a 502 from the edge, a refused connect) says nothing — the pod's status
// does. A lost session gets the dedicated lost code and is never re-run; an
// exited one reports the child's code. ok is false while the pod is not
// terminal, and the caller reports the dial error itself.
func terminalDialFailure(cmd *cobra.Command, verb, id string, pod sessionPod) (err error, ok bool) {
	at := "an unknown time"
	code := 0
	if t := pod.Terminated; t != nil {
		if strings.TrimSpace(t.FinishedAt) != "" {
			at = t.FinishedAt
		}
		code = t.ExitCode
	}
	switch pod.Phase {
	case cluster.PhaseLost:
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s lost: executor gone at %s; not re-run — re-run it explicitly\n", verb, id, at)
		return &exitStatusError{code: sessionLostExitCode}, true
	case cluster.PhaseSucceeded, cluster.PhaseFailed:
		if pod.Phase == cluster.PhaseFailed && code == 0 {
			code = 1
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks %s: session %s exited with code %d at %s; its session server is gone\n", verb, id, code, at)
		if code != 0 {
			return &exitStatusError{code: code}, true
		}
		return nil, true
	}
	return nil, false
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
		if verdict, ok := terminalDialFailure(cmd, "attach", id, tgt.pod); ok {
			return verdict
		}
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
	headers, err := env.authHeaders(raw)
	if err != nil {
		return fail(err)
	}
	conn, err := env.snapshotDial(ctx, raw, headers)
	if err != nil {
		if verdict, ok := terminalDialFailure(cmd, "logs", id, tgt.pod); ok {
			return verdict
		}
		return fail(planeError(sessionTargetCtx(tgt), err))
	}
	defer conn.Close()
	if err := readSnapshotToEnd(conn, cmd.OutOrStdout()); err != nil {
		return fail(err)
	}
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
		if verdict, ok := terminalDialFailure(cmd, "logs", id, tgt.pod); ok {
			return verdict
		}
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
	linger  time.Duration
}

// defaultSessionLinger is how long an exited session stays attachable: a
// client that disconnected before the child exited can still come back for
// the replay and the recorded exit code (contract §4.3).
const defaultSessionLinger = 10 * time.Minute

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
	cmd.Flags().DurationVar(&o.linger, "linger", defaultSessionLinger, "keep serving read-only replay and the exit code this long after the child exits")
	return cmd
}

// runSessionServer serves one session to its end: bind the attach port, run
// argv under a PTY into a fresh Hub, serve the attach protocol until the
// child exits, record the exit exactly once, linger, and exit with the
// child's code. A client disconnect detaches (the child keeps running). After
// the exit the server keeps serving for --linger so a late client still gets
// the replay and the recorded code; the attach handler serves an exited
// session read-only (no takeover, no input). The linger ends early on
// SIGTERM/SIGINT — `dks stop` deletes the Pod — and the process still exits
// with the child's code, so the Pod's terminal state carries it.
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
	if o.linger < 0 {
		return fail(fmt.Errorf("--linger %s is negative", o.linger))
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
	if o.linger > 0 {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
		select {
		case <-env.lingerAfter(o.linger):
		case <-stop:
		}
		signal.Stop(stop)
	}
	if code != 0 {
		return &exitStatusError{code: code}
	}
	return nil
}

// realDialStream is the production attach dial: pkg/dagrun/attach's
// reconnecting client. The paired client's auth rides whatever dialer the
// pairing story installs in this seam; this file sends no credentials.
func realDialStream(ctx context.Context, endpoint, clientID string, since int64, viewer bool, headers func() (http.Header, error)) (attachStream, error) {
	opts := []attach.Option{attach.Headers(headers)}
	if viewer {
		opts = append(opts, attach.Viewer())
	}
	s, err := attach.Attach(ctx, endpoint, clientID, since, opts...)
	if err != nil {
		return nil, describeHandshake(ctx, err, endpoint, headers, nil)
	}
	return &describedStream{Stream: s, endpoint: endpoint, headers: headers}, nil
}

// describedStream is the attach stream whose redial failures carry the same
// handshake diagnosis as the first dial.
type describedStream struct {
	*attach.Stream
	endpoint string
	headers  func() (http.Header, error)
}

func (d *describedStream) Reconnect(ctx context.Context) error {
	if err := d.Stream.Reconnect(ctx); err != nil {
		return describeHandshake(ctx, err, d.endpoint, d.headers, nil)
	}
	return nil
}

// describeHandshake turns gorilla's bare "websocket: bad handshake" into a
// diagnosable error: the HTTP status the edge answered and the dialed path.
// It never includes the query, userinfo or headers (no credentials). When
// the failed dial's response is not in hand (pkg/dagrun/attach does not
// surface it), one read-only viewer probe with the same headers recovers
// the status; a probe that unexpectedly upgrades is closed at once.
func describeHandshake(ctx context.Context, err error, endpoint string, headers func() (http.Header, error), resp *http.Response) error {
	if err == nil || !errors.Is(err, websocket.ErrBadHandshake) {
		return err
	}
	path := endpoint
	if u, perr := url.Parse(endpoint); perr == nil {
		path = u.Path
	}
	if resp == nil {
		resp = probeHandshake(ctx, endpoint, headers)
	}
	if resp == nil {
		return fmt.Errorf("%w (dialing %s; the edge's HTTP status is unknown)", err, path)
	}
	return fmt.Errorf("%w: HTTP %d %s (dialing %s)", err, resp.StatusCode, strings.TrimSpace(http.StatusText(resp.StatusCode)), path)
}

// probeHandshake redials endpoint as a viewer, only to read the response of
// a refused handshake. nil means no HTTP response was obtained.
func probeHandshake(ctx context.Context, endpoint string, headers func() (http.Header, error)) *http.Response {
	raw, err := snapshotURL(endpoint, "handshake-probe")
	if err != nil {
		return nil
	}
	var h http.Header
	if headers != nil {
		if h, err = headers(); err != nil {
			return nil
		}
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(ctx, raw, h)
	if err == nil {
		_ = ws.Close()
		return nil
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return resp
}

// realSnapshotDial opens the snapshot websocket carrying the paired
// client's headers.
func realSnapshotDial(ctx context.Context, endpoint string, headers http.Header) (snapshotConn, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, resp, err := dialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, describeHandshake(ctx, err, endpoint, nil, resp)
	}
	return ws, nil
}

// tunnelBaseEnv overrides the paired plane's matrix-tunnel base URL.
const tunnelBaseEnv = "BASHY_DKS_TUNNEL_BASE"

// defaultTunnelBase is $BASHY_DKS_TUNNEL_BASE when set, else the
// scheme://host of the plane context's cluster server in the kubeconfig:
// the paired plane fronts its Kubernetes API and its matrix tunnel on one
// edge. A context with no usable server fails closed.
func defaultTunnelBase(ctxRec dagrun.Context) (string, error) {
	if base := strings.TrimSuffix(strings.TrimSpace(os.Getenv(tunnelBaseEnv)), "/"); base != "" {
		return base, nil
	}
	kctx, err := planeContext(ctxRec.Plane)
	if err != nil {
		return "", err
	}
	server, err := kubeconfigServer(kctx)
	if err != nil {
		return "", fmt.Errorf("no matrix tunnel base: %v; set $%s to the paired plane base — nothing ran", err, tunnelBaseEnv)
	}
	u, err := url.Parse(server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("no matrix tunnel base: cluster server %q is not an http(s) URL; set $%s to the paired plane base — nothing ran", server, tunnelBaseEnv)
	}
	return u.Scheme + "://" + u.Host, nil
}

// kubeconfig is the slice of a kubeconfig file the base derivation reads.
type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// kubeconfigServer resolves kctx ("" = current-context) to its cluster
// server across $KUBECONFIG (else ~/.kube/config), merging the way kubectl
// does: the first file to set a field or name an entry wins.
func kubeconfigServer(kctx string) (string, error) {
	var files []string
	if v := os.Getenv("KUBECONFIG"); v != "" {
		files = filepath.SplitList(v)
	} else if home, err := os.UserHomeDir(); err == nil {
		files = []string{filepath.Join(home, ".kube", "config")}
	}
	current := ""
	contexts := map[string]string{}
	servers := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var kc kubeconfig
		if yaml.Unmarshal(b, &kc) != nil {
			continue
		}
		if current == "" {
			current = kc.CurrentContext
		}
		for _, c := range kc.Contexts {
			if _, ok := contexts[c.Name]; !ok {
				contexts[c.Name] = c.Context.Cluster
			}
		}
		for _, c := range kc.Clusters {
			if _, ok := servers[c.Name]; !ok {
				servers[c.Name] = c.Cluster.Server
			}
		}
	}
	if kctx == "" {
		kctx = current
	}
	if kctx == "" {
		return "", errors.New("kubeconfig names no current context")
	}
	cluster, ok := contexts[kctx]
	if !ok {
		return "", fmt.Errorf("kubeconfig has no context %q", kctx)
	}
	server := strings.TrimSpace(servers[cluster])
	if server == "" {
		return "", fmt.Errorf("kubeconfig context %q has no cluster server", kctx)
	}
	return server, nil
}

// elevationCookie is the edge's per-(host, app) authorization cookie.
const elevationCookie = "matrix_elev"

// pairedAuthHeaders authorizes one tunnel endpoint as the paired Outpost's
// account. A vk session pod is published as an authentication-only app, so
// the edge mints its app-scoped cookie for any request that authenticates
// as the host's owner or sharee — no OS password. One bearer GET to the app
// root collects the cookie; the dial then carries bearer and cookie. The
// credentials go only to the paired server's host: a tunnel elsewhere gets
// no headers at all.
func pairedAuthHeaders(endpoint string) (http.Header, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cred, ok := loadPairedCredential()
	if !ok || !strings.EqualFold(u.Hostname(), cred.host) {
		return nil, nil
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+cred.bearer)
	appRoot, host, app := appRootURL(u)
	req, err := http.NewRequest(http.MethodGet, appRoot, nil)
	if err != nil {
		return nil, err
	}
	req.Header = h.Clone()
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("authorize app %s on executor host %s: %w", app, host, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("the paired account cannot reach app %s on executor host %s (HTTP %d): the host must be owned by or shared with the paired account — nothing ran",
			app, host, resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == elevationCookie && c.Value != "" {
			h.Set("Cookie", (&http.Cookie{Name: elevationCookie, Value: c.Value}).String())
		}
	}
	return h, nil
}

// appRootURL maps a ws(s) attach endpoint onto the http(s) root of its app
// (…/app/<name>/), plus the host and app names for messages.
func appRootURL(u *url.URL) (root, host, app string) {
	r := *u
	r.RawQuery, r.Fragment = "", ""
	switch r.Scheme {
	case "ws":
		r.Scheme = "http"
	case "wss":
		r.Scheme = "https"
	}
	p := strings.TrimSuffix(r.Path, "/attach")
	r.Path = strings.TrimSuffix(p, "/") + "/"
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		switch parts[i] {
		case "h":
			host = parts[i+1]
		case "app":
			app = parts[i+1]
		}
	}
	return r.String(), host, app
}

// pairedCredential is the paired Outpost's bearer and the server it pairs.
type pairedCredential struct {
	bearer string
	host   string
}

// loadPairedCredential reads the paired Outpost's config file
// ($XDG_CONFIG_HOME/matrix/agent.json, else ~/.config/matrix/agent.json):
// the bearer is $OUTPOST_SESSION_JWT, else access_token, else token — the
// order outpost's own clients use; the server is server_addr. A file other
// users can read is refused.
func loadPairedCredential() (pairedCredential, bool) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return pairedCredential{}, false
		}
		dir = filepath.Join(home, ".config")
	}
	p := filepath.Join(dir, "matrix", "agent.json")
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return pairedCredential{}, false
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return pairedCredential{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return pairedCredential{}, false
	}
	var f struct {
		ServerAddr  string `json:"server_addr"`
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(b, &f) != nil {
		return pairedCredential{}, false
	}
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = strings.TrimSpace(f.AccessToken)
	}
	if bearer == "" {
		bearer = strings.TrimSpace(f.Token)
	}
	host := strings.TrimSpace(f.ServerAddr)
	if strings.Contains(host, "://") {
		if u, err := url.Parse(host); err == nil {
			host = u.Hostname()
		}
	} else if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if bearer == "" || host == "" || strings.ContainsAny(bearer, "\r\n") {
		return pairedCredential{}, false
	}
	return pairedCredential{bearer: bearer, host: host}, true
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
