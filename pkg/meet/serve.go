package meet

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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/coopauth"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// The HTTP + WebSocket surface — the room, reachable from a browser.
//
// `observe` (see observe.go) lets a human on the SAME HOST watch a meeting by tailing its two
// files (transcript.jsonl = the record, live.jsonl = the view). That is one participation
// surface: local, file-based. It is not the only one a room should have — a browser, a phone,
// or an operator on another machine must be able to attach too, and none of them can tail a
// file on this host.
//
// This is the second surface, and the design rule is that it must be a surface over the SAME
// source of truth, not a parallel one. So `meet serve` streams exactly the events `observe`
// reads — the same transcript backlog, then the same tailed record+view — over a WebSocket.
// The JSONL event schema IS the wire protocol; the socket is just a second transport for it.
// A frame a WS client receives is identical to a line `observe` would print. Add a transport,
// never a second truth.
//
// The same rule governs the WRITE half added here. Every /api route is a thin translation of
// an HTTP request into one call in api.go — the identical function the cobra tree calls. No
// route decides anything the CLI does not; the only thing this file owns is the mapping from
// Go errors to status codes and from Go values to JSON.
//
// Two properties of the room shape the routes, and both are consequences of the engine rather
// than choices made here:
//
//   - A POST is not a long-poll. Round, poll, ask, address and converge run agent CLIs for
//     minutes under the room's run lease. They answer 202 with a JobRef and do the work in a
//     goroutine; the OUTPUT arrives on /observe, because the transcript is where the room's
//     output has always gone. A second progress channel would be a second truth.
//   - The lease is checked BEFORE the 202, not inside the goroutine, so a room that is busy
//     answers 409 on the request the caller can still see. See startJob.
//
// This is S2 of docs/plan-observable-steward-sessions.md and track B of
// dhnt/docs/meet-web-chatroom-design.md. It deliberately reuses resolveMeeting / loadState /
// storeDir / lineTail / readEvents / readLive — the exact machinery observe uses — so the two
// surfaces cannot drift.

// defaultServePort is bashy-meet's loopback port. Registered with outpost as a
// cooperative app; never exposed directly.
const defaultServePort = 8637

// otelServiceName is this surface's identity on the OTel plane — the umbrella's
// trace-propagation contract names one service.name per tier, and a span that
// arrives as "unknown_service" cannot be correlated with the cloudbox-hub and
// outpost hops that produced it.
const otelServiceName = "bashy-meet"

func newServeCmd() *cobra.Command {
	var (
		port int
		bind string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "serve the room over HTTP + WebSocket, so a browser / phone / remote client can attach",
		Long: "Expose meetings over HTTP and a WebSocket so participation surfaces beyond the\n" +
			"local TUI (a browser, a mobile app, an operator on another machine) can attach,\n" +
			"watch live, and take part. It streams the SAME transcript + live events that\n" +
			"`bashy meet observe` tails and drives the SAME verbs `bashy meet` runs — one\n" +
			"source of truth, a second transport.\n\n" +
			"  GET  /                                      the embedded web room (SPA)\n" +
			"  ws://<host>:<port>/observe?room=<ROOM|id>   read-only live stream\n" +
			"  GET  /api/rooms                             the room list\n" +
			"  GET  /api/agents                            registered roster choices\n" +
			"  POST /api/rooms/<ref>/post                  say something\n" +
			"  GET  /healthz                               liveness\n\n" +
			"Reached directly on loopback it is ungated — that is the local development path.\n" +
			"Reached through outpost it requires a vouched identity (coopauth).",
		RunE: func(cmd *cobra.Command, args []string) error {
			surface := cmd.Root().Name()
			if surface != "relay" {
				surface = "meet"
			}
			return runMeetServe(cmd.Context(), cmd.OutOrStdout(), surface, bind, port)
		},
	}
	cmd.Flags().IntVar(&port, "port", defaultServePort, "listen port")
	cmd.Flags().StringVar(&bind, "bind", "127.0.0.1", "bind address")
	return cmd
}

func runMeetServe(ctx context.Context, out io.Writer, surface, bind string, port int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// Once, here, rather than per-verb: every agent this server spawns is inside a
	// meeting, and must be refused if it tries to convene its own panel. markDepth
	// INCREMENTS, so calling it per request would climb without bound and stamp a
	// nonsense depth onto every child.
	markDepth()
	if _, err := EnsureConfiguredPermanentRooms(); err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", bind, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServeHandler(ctx, MountOptions{}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Fprintf(out, "%s serve → http://%s/  (Bashy Meet; channels + direct messages)\n", surface, addr)
	if spaFS == nil {
		fmt.Fprintf(out, "%s serve → ⚠ built without the web UI; the API and live streams work, `/` explains how to get the SPA\n", surface)
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	case err := <-errc:
		return err
	}
}

// newServeHandler builds the whole surface. It is separate from runMeetServe so a
// test can drive every route through httptest without binding a port.
//
// ctx is the SERVER's lifetime, not a request's. Background jobs take their
// context from here: a request context is cancelled the moment the 202 is
// written, which would kill the very work the 202 promised.
func newServeHandler(ctx context.Context, opts MountOptions) http.Handler {
	mux := http.NewServeMux()
	guard := serveGuard()

	// An embedder that has already authenticated the caller injects its own gate.
	// See MountOptions for why mounting under a path prefix makes this necessary.
	roomGate := opts.Gate
	if roomGate == nil {
		roomGate = func(h http.Handler) http.Handler { return gate(guard, h.ServeHTTP) }
	}

	// /healthz is the only ungated route: it is the CI/CD liveness probe, it names
	// no room, and a probe that needs an identity is a probe that cannot run.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": otelServiceName})
	})

	// Everything room-shaped carries the gate, reads included.
	//
	// The contract only requires it on the WRITES, and the reads were briefly left
	// open on the reasoning that they change nothing. They disclose everything: a
	// transcript is the whole conversation, and /observe streams it live. That is
	// the same exposure the origin check on the socket exists to close, so leaving
	// the HTTP half of it open would fix one door and leave the other ajar. The
	// gate costs a loopback caller nothing — see gate.
	route := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, roomGate(h))
	}
	route("/observe", handleObserveWS)
	route("GET /api/rooms", handleRoomList)
	route("GET /api/agents", handleAgentList)
	route("GET /api/dms", handleRelayDMList)
	route("POST /api/dms", handleRelayDMList)
	route("GET /api/dms/{agent}", handleRelayDMGet)
	route("POST /api/dms/{agent}/messages", handleRelayDMMessage(ctx))
	route("POST /api/dms/{agent}/work", handleRelayDMWork(ctx))
	route("POST /api/dms/{agent}/recall", handleRelayDMRecall)
	route("/observe-dm", handleRelayDMObserve)
	route("GET /api/dms/{agent}/console", handleConsoleDM)
	route("GET /api/rooms/{ref}", handleRoomGet)
	route("POST /api/rooms", handleRoomCreate)
	route("POST /api/rooms/{ref}/post", handlePost)
	route("POST /api/rooms/{ref}/invite", handleInvite)
	route("POST /api/rooms/{ref}/kick", handleKick)
	route("POST /api/rooms/{ref}/mark", handleMark)
	route("POST /api/rooms/{ref}/address", handleAsync(ctx, "address"))
	route("POST /api/rooms/{ref}/recall", handleRecall)
	route("POST /api/rooms/{ref}/round", handleAsync(ctx, "round"))
	route("POST /api/rooms/{ref}/poll", handleAsync(ctx, "poll"))
	route("POST /api/rooms/{ref}/ask", handleAsync(ctx, "ask"))
	route("POST /api/rooms/{ref}/converge", handleAsync(ctx, "converge"))
	route("POST /api/rooms/{ref}/open", handleOpen)
	route("POST /api/rooms/{ref}/close", handleClose)

	// Everything else is the SPA, including its client-side routes.
	mux.HandleFunc("/", handleSPA)

	// otelhttp is wrapped unconditionally and costs nothing without an endpoint:
	// with no OTLP exporter configured the global provider is a no-op, so this is
	// span-shaped bookkeeping that never leaves the process. Telemetry is never a
	// hard dependency of serving a room.
	return otelhttp.NewHandler(mux, otelServiceName)
}

// serveGuard builds the cooperative-auth policy for the room routes.
//
// The secret comes from the environment because `meet serve` has no config file
// of its own; with no secret set the surface is loopback-only in practice, which
// is the local-first default the design insists on (§5: the whole feature works
// at http://127.0.0.1:<port> with no cloudbox, no pairing, and no account).
func serveGuard() *coopauth.Guard {
	secret := strings.TrimSpace(os.Getenv("BASHY_MEET_SSO_SECRET"))
	return &coopauth.Guard{
		Policy: coopauth.Policy{
			Secret:      []byte(secret),
			RequireHMAC: secret != "",
		},
	}
}

// gate is the two-entrance rule this app is reached by.
//
//   - Directly on loopback: no gate. That is the machine owner at their own
//     keyboard, and it is the whole development path. coopauth.Guard deliberately
//     will NOT admit this caller (its verify() treats only cloud-arrived requests
//     as authenticated), so a bare RequireAuth would lock the owner out of their
//     own room.
//   - Through outpost: X-Forwarded-Prefix is present, and RequireAuth is what
//     decides — a vouched identity, HMAC-verified when a secret is configured.
//
// Anything else — a direct hit from off-host with no vouch — is refused. That is
// the case a naive "skip the gate when there is no prefix header" would wave
// through, and it is exactly the one that matters.
func gate(guard *coopauth.Guard, next http.HandlerFunc) http.Handler {
	protected := guard.RequireAuth(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if coopauth.ArrivedViaCloud(r) {
			protected.ServeHTTP(w, r)
			return
		}
		if !coopauth.IsLoopbackAddr(r.RemoteAddr) {
			writeErr(w, http.StatusForbidden, errors.New("meet: this room is reachable on loopback or through a vouched tunnel, not directly"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- JSON plumbing -----------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr answers with the one error shape the contract names. The status is
// derived from the error's SENTINEL, never from its prose — the messages are
// written for humans and get reworded; errors.Is does not.
func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// ErrWrongMode is returned when a room's TYPE forbids a verb — a board asked to
// run a round, poll, ask, or converge. It is the same class of failure as
// ErrMeetingBusy: the request is well-formed, the room's state refuses it, so it
// maps to 409 rather than 400. A 400 would tell the browser IT got the request
// wrong, when nothing about the request needs fixing.
var ErrWrongMode = errors.New("meet: the room's mode does not allow this")

// wrongModeError carries the engine's human-readable refusal (which names the
// room and the recovery) while classifying as ErrWrongMode for the status map.
type wrongModeError struct{ cause error }

func (e wrongModeError) Error() string   { return e.cause.Error() }
func (e wrongModeError) Unwrap() []error { return []error{ErrWrongMode, e.cause} }

// apiErr classifies an engine error into the contract's status codes.
//
//	ErrMeetingBusy   409 — somebody else holds the room's floor; retry
//	ErrWrongMode     409 — the room's mode forbids this verb (a board's floor
//	                       is never run for it); not retryable, but the request
//	                       was well-formed and the fault is the room's state
//	ErrNotOrganizer  403 — a member tried an organizer's act
//	ErrNoRoom        404 — the reference names no meeting here
//
// Everything else is 400: by construction the remaining failures are refusals
// from Validate, routableSeat, or an empty operand, and every one of them is
// something the CALLER got wrong. A blanket 500 would tell a browser to retry a
// request that will never succeed.
func apiErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrMeetingBusy):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, ErrWrongMode):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, ErrNotOrganizer):
		writeErr(w, http.StatusForbidden, err)
	case errors.Is(err, ErrNoRoom):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, ErrDeclined):
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusBadRequest, err)
	}
}

// decodeBody reads a JSON request body into v. A missing body is not an error —
// several routes (round, converge) take no arguments at all, and demanding `{}`
// from them would be ceremony.
func decodeBody(r *http.Request, v any) error {
	defer func() { _, _ = io.Copy(io.Discard, r.Body) }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("meet: malformed request body: %w", err)
	}
	return nil
}

// actorOf names who is acting, for the privilege checks.
//
// A cloud-vouched caller is named by its VERIFIED identity and the body is
// ignored — a room where the organizer check could be passed by typing somebody
// else's name into JSON would not have an organizer check at all. Only the
// ungated loopback path may say who it is (the CLI's `--as`), because that caller
// has already proved the only thing loopback can prove: it is the machine owner.
//
// The default is THIS HOST's human, never the room's. Defaulting to the room's
// own human would make requireOrganizer vacuous: every request would arrive
// already wearing the name of whoever convened the room it is addressing.
func actorOf(r *http.Request) string {
	// WHO IS ACTING COMES FROM THE REQUEST, NEVER FROM THE BODY.
	//
	// This used to accept a caller-stated name on any ungated path, which is the
	// hole the console had already closed on its own send path: "a browser that
	// could name its own from could sign as any agent on the host"
	// (pkg/webconsole/panel_mb.go). /meet/ is a panel of that same console, so a
	// page could be signed one way in Messages and another way here.
	//
	// It also took the cloud identity's User — the EMAIL — where the console
	// takes Username, the collision-free app handle. One cloud human was
	// therefore two names on one page load, which fragments their inbox and
	// makes attribution unreliable. Same source, same field, same
	// canonicalization as pkg/webconsole/api.go userOf.
	var user string
	if id, ok := coopauth.IdentityOf(r); ok {
		user = strings.TrimSpace(id.Username)
		if user == "" {
			return "" // vouched but unnamed: let requireOrganizer refuse it
		}
	}
	if user == "" {
		user = humanName()
	}
	if canon, err := bus.BoardIdentity(user); err == nil && strings.TrimSpace(canon) != "" {
		return canon
	}
	return user
}

// --- Room routes -------------------------------------------------------------

func handleRoomList(w http.ResponseWriter, r *http.Request) {
	rooms, err := Rooms()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, rooms)
}

func handleAgentList(w http.ResponseWriter, r *http.Request) {
	agents, errs := registeredAgentOptions()
	if len(errs) > 0 {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("meet: read agent catalog: %w", errors.Join(errs...)))
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

func handleRoomGet(w http.ResponseWriter, r *http.Request) {
	st, syn, err := Room(r.PathValue("ref"))
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": viewOf(st), "synthesis": syn})
}

// handleRoomCreate opens a room in the NAME OF WHOEVER ASKED FOR IT.
//
// Create is the one route where the caller's identity is not checked against a
// roster but becomes one: there is no room yet, so there is nobody it could be
// checked against. Through the tunnel that identity is a cloudbox account — an
// email — and it is seated as the room's human, which is both true (the person
// who opened the room is in it) and what every later privilege check needs, since
// requireOrganizer has nothing to compare against but the name the room recorded.
//
// actorOf decides who that is on the same terms as every other route: a vouched
// caller is its VERIFIED identity and the body is ignored; only ungated loopback —
// which has already proved it is the machine owner — may say who it is.
func handleRoomCreate(w http.ResponseWriter, r *http.Request) {
	// This is deliberately smaller than CreateOptions. A browser may choose who
	// facilitates and participates; it may not smuggle in lifecycle, output, or
	// launch controls that belong to the local CLI.
	var body struct {
		Topic        string   `json:"topic"`
		Participants []string `json:"participants"`
		Owner        string   `json:"owner"`
		Human        string   `json:"human"`
	}
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	owner := canonAgent(body.Owner)
	if strings.TrimSpace(owner) == "" {
		apiErr(w, errors.New("meet: a new meeting needs an explicit facilitator/owner"))
		return
	}
	if len(body.Participants) == 0 {
		apiErr(w, errors.New("meet: a new meeting needs at least one participant"))
		return
	}
	if err := routableSeat(owner); err != nil {
		apiErr(w, fmt.Errorf("meet: facilitator/owner: %w", err))
		return
	}
	opts := CreateOptions{
		Topic: body.Topic, Participants: body.Participants, Chair: owner,
		NoSecretary: true, Human: actorOf(r),
	}
	if strings.TrimSpace(opts.Human) == "" {
		// Vouched but unnamed. Falling back to this host's user would file the room
		// under the machine owner and hand them an organizer privilege the actual
		// caller then could not exercise — a room nobody present can close.
		writeErr(w, http.StatusForbidden, errors.New(
			"meet: this request is vouched for but carries no account name, so there is nobody to open the room as"))
		return
	}
	// A browser-created meeting is convened by its authenticated human. The
	// facilitator runs the floor; it does not inherit the organizer's authority.
	opts.Initiator = opts.Human
	st, err := Create(opts)
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handlePost attributes a message rather than authorizing one — any member may
// post, so there is no privilege to check and no reason to invent a name. A
// vouched caller is credited with its verified identity; an ungated loopback
// caller may say who it is, and an unstated one falls through to the ROOM's own
// human, which is what `meet tell` does.
// handlePost appends a human contribution, optionally ADDRESSED.
//
// `to` was dropped here for as long as this route existed, so the browser could
// only ever post mail addressed to nobody — visible in the transcript, in no
// participant's actionable inbox, and waking no one. The CLI could already say
// it (`meet tell --to`); the room's own web UI could not. `all` broadcasts.
func handlePost(w http.ResponseWriter, r *http.Request) {
	var body struct{ Author, Text, To string }
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	author := strings.TrimSpace(body.Author)
	if id, ok := coopauth.IdentityOf(r); ok {
		author = strings.TrimSpace(id.User)
	}
	ev, err := PostAs(r.PathValue("ref"), author, body.To, body.Text)
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// handleRecall is "stop that message" for a room.
//
// The caller sends whichever handle it has — the job id from its own 202, the
// timestamp of the record it can see, or both — and the SERVER says what that
// achieved. It never says "canceled" for a message that went out: see recall.go
// for why that distinction cannot be made in a browser.
func handleRecall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Job string `json:"job"`
		TS  string `json:"ts"`
	}
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	st, err := roomOf(r.PathValue("ref"))
	if err != nil {
		apiErr(w, err)
		return
	}
	// The retraction is attributed to the caller, resolved the same way a post
	// is: a withdrawal signed by anyone but the sender is not a withdrawal.
	res, err := Recall(st, body.Job, body.TS, actorOf(r))
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func handleInvite(w http.ResponseWriter, r *http.Request) {
	handleRoster(w, r, Invite)
}

func handleKick(w http.ResponseWriter, r *http.Request) {
	handleRoster(w, r, Kick)
}

// handleRoster is invite and kick, which differ only in the verb.
//
// The room is resolved first so an unknown reference is a 404 rather than
// whatever Invite/Kick happen to say: those live in roster.go and reach the room
// through loadMeeting, which has no reason to know about HTTP status codes. This
// is the classification, not a second lookup of any consequence.
func handleRoster(w http.ResponseWriter, r *http.Request, verb func(ref, actor, agent string) error) {
	var body struct{ Agent, Actor string }
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	ref := r.PathValue("ref")
	if _, err := roomOf(ref); err != nil {
		apiErr(w, err)
		return
	}
	if err := verb(ref, actorOf(r), body.Agent); err != nil {
		apiErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleMark(w http.ResponseWriter, r *http.Request) {
	var body struct{ Kind, Text string }
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	ev, err := Mark(r.PathValue("ref"), body.Kind, body.Text)
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

func handleClose(w http.ResponseWriter, r *http.Request) {
	var body struct{ Actor string }
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	if err := Close(r.PathValue("ref"), actorOf(r)); err != nil {
		apiErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleOpen(w http.ResponseWriter, r *http.Request) {
	var body struct{ Actor string }
	if err := decodeBody(r, &body); err != nil {
		apiErr(w, err)
		return
	}
	st, err := Open(r.PathValue("ref"), actorOf(r))
	if err != nil {
		apiErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// --- The long verbs ----------------------------------------------------------

// asyncBody is the union of what round/poll/ask/address/converge take. They share
// a handler because they share the ONE thing that makes them different from every
// other route: they run agent CLIs for minutes and must not hold a request open
// while they do.
type asyncBody struct {
	Agent    string   `json:"agent"`
	Text     string   `json:"text"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

func handleAsync(srvCtx context.Context, verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body asyncBody
		if err := decodeBody(r, &body); err != nil {
			apiErr(w, err)
			return
		}
		ref := r.PathValue("ref")
		st, err := roomOf(ref)
		if err != nil {
			apiErr(w, err)
			return
		}

		// A board's floor is never run for it, so refuse HERE — before startJob.
		// Deferring to the engine's own guard would hand the browser a 202 for
		// work that then dies where nobody is looking, which is the exact
		// fire-and-forget-that-fails-silently shape startJob's probe exists to
		// prevent, now at the mode level rather than the lease level.
		if st.board() {
			switch verb {
			case "round":
				apiErr(w, wrongModeError{st.boardRefusal("run a round")})
				return
			case "poll":
				apiErr(w, wrongModeError{st.boardRefusal("run a poll")})
				return
			case "ask":
				apiErr(w, wrongModeError{st.boardRefusal("put a question to the room")})
				return
			case "converge":
				apiErr(w, wrongModeError{st.boardRefusal("run a synthesis pass")})
				return
			}
		}

		var run func(context.Context, *liveJob) error
		switch verb {
		case "address":
			if strings.TrimSpace(body.Agent) == "" {
				apiErr(w, errors.New("meet: address needs an agent"))
				return
			}
			run = func(ctx context.Context, j *liveJob) error {
				_, err := addressJob(ctx, ref, body.Agent, body.Text, j)
				return err
			}
		case "round":
			run = func(ctx context.Context, _ *liveJob) error { _, err := Round(ctx, ref); return err }
		case "poll":
			if strings.TrimSpace(body.Question) == "" {
				apiErr(w, errors.New("meet: a poll needs a question"))
				return
			}
			run = func(ctx context.Context, _ *liveJob) error {
				_, err := Poll(ctx, ref, body.Question, body.Choices)
				return err
			}
		case "ask":
			if strings.TrimSpace(body.Question) == "" {
				apiErr(w, errors.New("meet: an open question needs a question"))
				return
			}
			run = func(ctx context.Context, _ *liveJob) error { _, err := Ask(ctx, ref, body.Question); return err }
		case "converge":
			run = func(ctx context.Context, _ *liveJob) error { _, err := Converge(ctx, ref); return err }
		default:
			writeErr(w, http.StatusNotFound, fmt.Errorf("meet: no such verb %q", verb))
			return
		}

		job, err := startJob(srvCtx, st.ID, verb, run)
		if err != nil {
			apiErr(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	}
}

var jobSeq atomic.Uint64

// startJob answers the caller now and does the work after.
//
// The lease is PROBED here — taken and immediately released — rather than in the
// goroutine, and that is the whole reason a busy room gets a clean 409 instead of
// a 202 for work that then fails where nobody is looking. The probe is genuinely
// a probe: the goroutine acquires the lease for real (inside runRound / runPoll /
// runAsk / Address), because holding it across the handoff is impossible — flock
// is per file description, and a second acquire in this same process would
// contend with our own.
//
// So there is a microsecond-wide window in which another runner can win between
// the probe and the real acquire. That is not papered over: the loser records its
// refusal IN THE ROOM, where the caller is already watching.
func startJob(srvCtx context.Context, id, verb string, run func(context.Context, *liveJob) error) (JobRef, error) {
	lease, err := acquireRunLease(id)
	if err != nil {
		return JobRef{}, err
	}
	lease.Release()

	job := fmt.Sprintf("%s-%d", verb, jobSeq.Add(1))
	// The server's lifetime, not the request's: a request context is cancelled
	// the instant the 202 is written, which would abort the work it promised.
	// Cancellable, so the SENDER can stop it — see recall.go. Nobody else holds
	// this cancel: a job ends when it finishes or when the person who started it
	// takes it back.
	parent := srvCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	j := registerJob(job, id, cancel)
	go func() {
		defer cancel()
		defer j.retire()
		err := run(ctx, j)
		switch {
		case err == nil:
			return
		case j.stopped():
			// The recall already recorded what happened — a cancelled/retracted
			// message plus a "did not run" note reads as two separate failures.
			return
		default:
			if st, lerr := loadState(id); lerr == nil {
				// The transcript is the room's only output channel, so a job that
				// failed says so there. A 202 followed by silence would leave the
				// room believing a round is still coming.
				_, _ = record(st, "note", otelServiceName, "",
					fmt.Sprintf("%s (job %s) did not run: %v", verb, job, err))
			}
		}
	}()
	return JobRef{ID: job, Room: id}, nil
}

// --- The WebSocket -----------------------------------------------------------

// wsFrame is the envelope every socket frame carries. `kind` distinguishes the RECORD
// (a whole-turn transcript Event) from the VIEW (a line-granular LiveEvent) — the same two
// channels observe keeps distinct, kept distinct here so a client parser is never handed two
// schemas with no tag to tell them apart.
type wsFrame struct {
	Kind string `json:"kind"` // "info" (the room header) | "event" (record) | "live" (view) | "history-end"
	Data any    `json:"data,omitempty"`
	Note string `json:"note,omitempty"`
}

var wsUpgrader = websocket.Upgrader{CheckOrigin: originAllowed}

// originAllowed is the socket's same-origin policy.
//
// It used to return true unconditionally, on the reasoning that origin policy is
// the portal's concern and the socket serves whoever the surface in front of it
// let through. That reasoning does not survive this change. The room is now
// reachable from a browser and its write routes are cookie/header-authenticated,
// so a page on any origin could open a socket that rides the visitor's ambient
// credentials and read a meeting it has no business seeing. The socket is
// read-only, which bounds the damage to disclosure — disclosure of the entire
// transcript.
//
// Three cases, in order:
//
//   - No Origin header at all: not a browser. A CLI, a Go client, a test. Browsers
//     always send one on a WebSocket handshake, so its absence cannot be a page.
//   - A loopback origin: the machine owner at their own keyboard, on whatever port
//     the SPA dev server happens to be using. This is the local development path
//     the design insists must work with no cloudbox and no pairing.
//   - Anything else must match the host the request arrived for. Behind outpost
//     that is X-Forwarded-Host (r.Host is the loopback hop, and comparing against
//     it would refuse every legitimate tunneled client).
func originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if isLoopbackHost(u.Hostname()) {
		return true
	}
	for _, h := range requestHosts(r) {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// requestHosts lists the hosts this request may legitimately have been made for,
// most-external first. X-Forwarded-Host may carry a comma-separated chain when
// more than one proxy is in front; the first entry is the one the browser typed.
func requestHosts(r *http.Request) []string {
	var out []string
	for h := range strings.SplitSeq(r.Header.Get("X-Forwarded-Host"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	if r.Host != "" {
		out = append(out, r.Host)
	}
	return out
}

func handleObserveWS(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("room")
	if ref == "" {
		http.Error(w, "missing ?room=<ROOM|id>", http.StatusBadRequest)
		return
	}
	id, err := resolveMeeting(ref)
	if err != nil {
		http.Error(w, "no such room: "+err.Error(), http.StatusNotFound)
		return
	}
	st, err := loadState(id)
	if err != nil {
		http.Error(w, "load room: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dir, err := storeDir(id)
	if err != nil {
		http.Error(w, "room dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error
	}
	defer conn.Close()

	// A read pump: gorilla only processes control frames (close/ping) while a reader runs,
	// and it is where client→room messages (tell/say) will arrive next. For now it just
	// drains, so a client disconnect is noticed promptly.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// ?raw=1 asks for the agent's un-normalized output alongside the prose, so an
	// operator debugging a harness can see the transport this seam dropped. Off by
	// default and per-connection: the browser reconnects with it when the reader
	// turns the debug view on, so an ordinary session never carries the bytes.
	debugRaw := truthy(r.URL.Query().Get("raw"))

	rec := &lineTail{path: filepath.Join(dir, "transcript.jsonl")}
	live := &lineTail{path: filepath.Join(dir, "live.jsonl")}
	// The view is followed forward only; its history is already in the record as whole turns.
	live.skipToEnd()

	writeFrame := func(f wsFrame) error {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(f)
	}

	// 0. The header, FIRST and before anything else.
	//
	// The state was already loaded (to prove the room exists) and then thrown away,
	// so a client learned nothing about what it had attached to: not the topic, not
	// the roster, not whether the room was still open, not which round it was on. A
	// UI cannot render a room from a stream of turns alone, and asking it to fetch
	// the header over a second channel would race the backlog it is about to be sent.
	if writeFrame(wsFrame{Kind: "info", Data: viewOf(st)}) != nil {
		return
	}

	// 1. Replay the record — complete and authoritative — so the client sees everything that
	//    happened before it attached, exactly as observe does.
	backlog, err := readEvents(rec)
	if err == nil {
		for _, e := range backlog {
			if writeFrame(wsFrame{Kind: "event", Data: renderEvent(e, debugRaw)}) != nil {
				return
			}
		}
	}
	if writeFrame(wsFrame{Kind: "history-end", Note: fmt.Sprintf("%d event(s) of history", len(backlog))}) != nil {
		return
	}

	// 2. Tail forward: new view lines and new record events, the same loop observe runs.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-closed:
			return
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if conn.WriteMessage(websocket.PingMessage, nil) != nil {
				return
			}
		case <-time.After(observePoll):
		}

		if lines, err := readLive(live); err == nil {
			for _, l := range lines {
				if writeFrame(wsFrame{Kind: "live", Data: l}) != nil {
					return
				}
			}
		}
		if events, err := readEvents(rec); err == nil {
			for _, e := range events {
				if writeFrame(wsFrame{Kind: "event", Data: renderEvent(e, debugRaw)}) != nil {
					return
				}
			}
		}

		// 3. Stop when the room does — the drain-then-stop observeMeeting runs.
		//
		// Re-read the header rather than trusting the copy we opened with: the
		// meeting is another process, and its status is the only thing that says
		// the stream has ended. This socket used to stream forever, so a browser
		// tab left open on a finished meeting held a goroutine and two file tails
		// until the process died.
		//
		// Drain FIRST, then stop. The closing turns are written before the status
		// flips, and exiting on the flag alone would truncate the last word — the
		// one that says what was decided.
		if cur, err := loadState(id); err == nil && cur.Status != "open" {
			if rest, err := readEvents(rec); err == nil {
				for _, e := range rest {
					if writeFrame(wsFrame{Kind: "event", Data: renderEvent(e, debugRaw)}) != nil {
						return
					}
				}
			}
			// A final info frame carries the closed header, so a client sees the
			// status change rather than inferring it from a socket that hung up.
			_ = writeFrame(wsFrame{Kind: "info", Data: viewOf(cur), Note: "meeting " + cur.Status})
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "meeting "+cur.Status),
				time.Now().Add(time.Second))
			return
		}
	}
}
