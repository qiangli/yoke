package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

var (
	newSessionClient = func(base, token string) SessionClient {
		return NewHTTPSessionClient(base, token)
	}
	sessionPollInterval = 2 * time.Second
)

type sessionRepoClient struct {
	repoRoot string
	pointer  *SessionPointer
	client   SessionClient
	// created reports that EnsureRepoSession minted the task on this call.
	created bool
}

// Pointer is the resolved session pointer (task id, repo key, seat).
func (sc *sessionRepoClient) Pointer() *SessionPointer { return sc.pointer }

func sessionClientForRepo() (*sessionRepoClient, error) {
	cwd, _ := os.Getwd()
	return sessionClientForRepoRoot(cwd)
}

func sessionClientForRepoRoot(repoRoot string) (*sessionRepoClient, error) {
	p, err := ReadSessionPointer(repoRoot)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("no shared session pointer found; join with an explicit task id first")
	}
	base, token, err := sessionCredentials(p)
	if err != nil {
		return nil, err
	}
	return &sessionRepoClient{
		repoRoot: repoRoot,
		pointer:  p,
		client:   newSessionClient(base, token),
	}, nil
}

func runWeaveSessions(cmd *cobra.Command, flags *weaveOutputFlags) error {
	const verb = "weave sessions"
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	tasks, err := sc.client.ListTasks(cmd.Context())
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, map[string]any{"tasks": tasks}))
	}
	renderSessionTasks(cmd.OutOrStdout(), tasks)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveJoin(cmd *cobra.Command, explicitTaskID string, observer, once bool, flags *weaveOutputFlags) error {
	const verb = "weave join"
	mode := flags.mode()
	cwd, _ := os.Getwd()
	var (
		sc     *sessionRepoClient
		taskID string
		joined JoinResponse
		err    error
	)
	if explicitTaskID == "" && !observer {
		// The derived path: the repo you stand in IS the session key.
		sc, err = EnsureRepoSession(cmd.Context(), cwd)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
		}
		taskID = sc.pointer.TaskID
		participant, host := SessionParticipant()
		joined, err = sc.client.Join(cmd.Context(), taskID, JoinReq{Participant: participant, Host: host, Tool: "bashy", Role: "contributor"})
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
		}
	} else {
		// The explicit path: an id someone handed over, or an observer seat.
		// No pointer is required — credentials come from the one ladder.
		pointer, perr := ReadSessionPointer(cwd)
		if perr != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, perr))
		}
		base, token, cerr := sessionCredentials(pointer)
		if cerr != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, cerr))
		}
		if pointer == nil {
			pointer = &SessionPointer{CloudboxBase: base, TokenRef: "CLOUDBOX_TOKEN"}
		}
		sc = &sessionRepoClient{repoRoot: cwd, pointer: pointer, client: newSessionClient(base, token)}
		taskID, err = resolveJoinTaskID(cmd.Context(), sc.client, explicitTaskID, sc.pointer)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitInvalidArg, err))
		}
		participant, host := SessionParticipant()
		role := "contributor"
		if observer {
			role = "observer"
		}
		joined, err = sc.client.Join(cmd.Context(), taskID, JoinReq{Participant: participant, Host: host, Tool: "bashy", Role: role})
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
		}
		sc.pointer.TaskID = taskID
		if sc.pointer.RepoKey == "" {
			if key, kerr := RepoKey(cwd); kerr == nil {
				sc.pointer.RepoKey = key
			}
		}
		if err := WriteSessionPointer(sc.repoRoot, sc.pointer); err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
		}
	}
	if mode == weavecli.OutputJSON {
		if once {
			return ec(emitOK(cmd.OutOrStdout(), mode, verb, joined))
		}
		_ = emitOK(cmd.OutOrStdout(), mode, verb, joined)
	} else {
		renderJoinResponse(cmd.OutOrStdout(), joined)
	}
	if once {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = sessionFeedLoop(ctx, cmd.OutOrStdout(), sc.client, taskID, joined.Cursor, 0, func(ctx context.Context) error {
		timer := time.NewTimer(sessionPollInterval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	return nil
}

func runWeaveNote(cmd *cobra.Command, text string, flags *weaveOutputFlags) error {
	return runSessionAppend(cmd, flags, "weave note", AppendEventReq{Kind: "note", Summary: text})
}

func runWeaveSteer(cmd *cobra.Command, run, text string, flags *weaveOutputFlags) error {
	detail, _ := json.Marshal(map[string]string{"run": run, "verb": "say", "arg": text})
	return runSessionAppend(cmd, flags, "weave steer", AppendEventReq{
		Kind:    "directive",
		Summary: text,
		Detail:  detail,
	})
}

func runSessionAppend(cmd *cobra.Command, flags *weaveOutputFlags, verb string, req AppendEventReq) error {
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	ev, err := appendSessionEvent(cmd.Context(), sc.client, taskID, req)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, ev))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "[%s] %s\n", ev.Kind, ev.Summary)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveTake(cmd *cobra.Command, holder string, force bool, ttl time.Duration, flags *weaveOutputFlags) error {
	const verb = "weave take"
	mode := flags.mode()
	if ttl < 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitInvalidArg, fmt.Errorf("--ttl must be non-negative")))
	}
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	resp, err := takeSessionLease(cmd.Context(), sc.client, taskID, holder, force, ttl)
	if errors.Is(err, ErrLeaseHeld) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitStateConflict, leaseHeldUserError(err)))
	}
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, resp))
	}
	renderLease(cmd.OutOrStdout(), resp)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveHandoff(cmd *cobra.Command, to string, flags *weaveOutputFlags) error {
	const verb = "weave handoff"
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	ev, lease, err := handoffSession(cmd.Context(), sc.client, taskID, to)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, map[string]any{"event": ev, "lease": lease}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "handoff recorded for %s; lease released\n", to)
	fmt.Fprintf(cmd.OutOrStdout(), "successor runs: weave take --as %s\n", to)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveRoster(cmd *cobra.Command, flags *weaveOutputFlags) error {
	const verb = "weave roster"
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	roster, err := sessionRoster(cmd.Context(), sc.client, taskID)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, roster))
	}
	renderRoster(cmd.OutOrStdout(), roster)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveShare(cmd *cobra.Command, email, role string, flags *weaveOutputFlags) error {
	const verb = "weave share"
	mode := flags.mode()
	if role == "" {
		role = "observer"
	}
	if role != "observer" && role != "contributor" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitInvalidArg, fmt.Errorf("--role must be observer or contributor")))
	}
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	share, err := sc.client.GrantShare(cmd.Context(), taskID, GrantShareReq{ShareeEmail: email, Role: role})
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, share))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "shared session with %s as %s\n", email, role)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveShares(cmd *cobra.Command, flags *weaveOutputFlags) error {
	const verb = "weave shares"
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	shares, err := sc.client.ListShares(cmd.Context(), taskID)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, shares))
	}
	for _, share := range shares {
		fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", share.ShareeEmail, share.Role)
	}
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func runWeaveUnshare(cmd *cobra.Command, email string, flags *weaveOutputFlags) error {
	const verb = "weave unshare"
	mode := flags.mode()
	sc, err := sessionClientForRepo()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	taskID, err := joinedTaskID(sc.pointer)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	if err := sc.client.RevokeShare(cmd.Context(), taskID, email); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked %s\n", email)
	return ec(emitOK(cmd.OutOrStdout(), mode, verb, nil))
}

func resolveJoinTaskID(ctx context.Context, client SessionClient, explicit string, pointer *SessionPointer) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if pointer != nil && pointer.TaskID != "" {
		return pointer.TaskID, nil
	}
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		return "", err
	}
	var active []TaskSummary
	for _, task := range tasks {
		if sessionTaskActive(task) {
			active = append(active, task)
		}
	}
	if len(active) == 1 {
		return active[0].ID, nil
	}
	if len(active) == 0 {
		return "", fmt.Errorf("no active session task found; provide a task id")
	}
	return "", fmt.Errorf("multiple active session tasks found; provide a task id")
}

func sessionTaskActive(task TaskSummary) bool {
	switch strings.ToLower(task.Status) {
	case "done", "failed", "cancelled", "canceled", "closed", "complete", "completed":
		return false
	default:
		return true
	}
}

func sessionFeedLoop(ctx context.Context, w io.Writer, client SessionClient, taskID, cursor string, maxPolls int, sleep func(context.Context) error) (string, error) {
	polls := 0
	for {
		resp, err := client.GetEvents(ctx, taskID, cursor, 100)
		if err != nil {
			return cursor, err
		}
		for _, ev := range resp.Events {
			fmt.Fprintf(w, "[%s] %s\n", ev.Kind, ev.Summary)
		}
		if resp.Cursor != "" {
			cursor = resp.Cursor
		} else if len(resp.Events) > 0 {
			cursor = resp.Events[len(resp.Events)-1].ID
		}
		polls++
		if maxPolls > 0 && polls >= maxPolls {
			return cursor, nil
		}
		if err := sleep(ctx); err != nil {
			return cursor, err
		}
	}
}

func appendSessionEvent(ctx context.Context, client SessionClient, taskID string, req AppendEventReq) (Event, error) {
	return client.AppendEvent(ctx, taskID, req)
}

func takeSessionLease(ctx context.Context, client SessionClient, taskID, holder string, force bool, ttl time.Duration) (LeaseResponse, error) {
	if holder == "" {
		holder, _ = SessionParticipant()
	}
	action := "claim"
	if force {
		action = "force"
	}
	req := LeaseReq{Action: action, Holder: holder}
	if ttl > 0 {
		secs := int(ttl.Seconds())
		if secs <= 0 {
			secs = 1
		}
		req.TTLSeconds = &secs
	}
	return client.Lease(ctx, taskID, req)
}

func leaseHeldUserError(err error) error {
	if holder := leaseHeldHolder(err); holder != "" {
		return fmt.Errorf("held by %s; use --force", holder)
	}
	return fmt.Errorf("lease held; use --force: %w", err)
}

func leaseHeldHolder(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if start := strings.Index(msg, "{"); start >= 0 {
		var body struct {
			Error struct {
				Holder      string `json:"holder"`
				LeaseHolder string `json:"lease_holder"`
			} `json:"error"`
			Holder      string `json:"holder"`
			LeaseHolder string `json:"lease_holder"`
		}
		if json.Unmarshal([]byte(msg[start:]), &body) == nil {
			switch {
			case body.Error.Holder != "":
				return body.Error.Holder
			case body.Error.LeaseHolder != "":
				return body.Error.LeaseHolder
			case body.Holder != "":
				return body.Holder
			case body.LeaseHolder != "":
				return body.LeaseHolder
			}
		}
	}
	const marker = "held by "
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	for j, r := range rest {
		switch r {
		case ';', ',', '\n', '\r', '\t', '"', '\'':
			return strings.TrimSpace(rest[:j])
		}
	}
	return strings.TrimSpace(rest)
}

func handoffSession(ctx context.Context, client SessionClient, taskID, to string) (Event, LeaseResponse, error) {
	detail, _ := json.Marshal(map[string]string{"to": to, "mode": "manual"})
	ev, err := client.AppendEvent(ctx, taskID, AppendEventReq{
		Kind:    "handoff",
		Summary: "handoff to " + to,
		Detail:  detail,
	})
	if err != nil {
		return Event{}, LeaseResponse{}, err
	}
	lease, err := client.Lease(ctx, taskID, LeaseReq{Action: "release"})
	if err != nil {
		return Event{}, LeaseResponse{}, err
	}
	return ev, lease, nil
}

type sessionRosterResult struct {
	TaskID       string   `json:"task_id"`
	Holder       *string  `json:"lease_holder"`
	Events       []Event  `json:"events"`
	Participants []string `json:"participants"`
}

func sessionRoster(ctx context.Context, client SessionClient, taskID string) (sessionRosterResult, error) {
	tasks, err := client.ListTasks(ctx)
	if err != nil {
		return sessionRosterResult{}, err
	}
	var holder *string
	for _, task := range tasks {
		if task.ID == taskID {
			holder = task.LeaseHolder
			break
		}
	}
	resp, err := client.GetEvents(ctx, taskID, "", 0)
	if err != nil {
		return sessionRosterResult{}, err
	}
	seen := map[string]bool{}
	if holder != nil && *holder != "" {
		seen[*holder] = true
	}
	var events []Event
	for _, ev := range resp.Events {
		switch ev.Kind {
		case "join", "leave", "handoff":
			events = append(events, ev)
			if p := joinParticipant(ev); p != "" {
				seen[p] = true
			}
			if ev.Kind == "handoff" {
				var d struct {
					To string `json:"to"`
				}
				if json.Unmarshal(ev.Detail, &d) == nil && d.To != "" {
					seen[d.To] = true
				}
			}
		}
	}
	participants := make([]string, 0, len(seen))
	for p := range seen {
		participants = append(participants, p)
	}
	sort.Strings(participants)
	return sessionRosterResult{TaskID: taskID, Holder: holder, Events: events, Participants: participants}, nil
}

// joinParticipant is the seat a presence event names: the envelope's
// participant when present (what bashy sends), else the summary, which
// cloudbox's join handler writes as "<participant> joined".
func joinParticipant(ev Event) string {
	var d struct {
		Participant string `json:"participant"`
	}
	if json.Unmarshal(ev.Detail, &d) == nil && strings.TrimSpace(d.Participant) != "" {
		return strings.TrimSpace(d.Participant)
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ev.Summary), " joined"))
}

func joinedTaskID(pointer *SessionPointer) (string, error) {
	if pointer == nil || pointer.TaskID == "" {
		return "", fmt.Errorf("no joined session task; run weave join <task-id>")
	}
	return pointer.TaskID, nil
}

func renderSessionTasks(w io.Writer, tasks []TaskSummary) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tLEASE_HOLDER\tGOAL")
	for _, task := range tasks {
		holder := ""
		if task.LeaseHolder != nil {
			holder = *task.LeaseHolder
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", task.ID, task.Status, holder, task.Goal)
	}
	_ = tw.Flush()
}

func renderJoinResponse(w io.Writer, joined JoinResponse) {
	holder := ""
	if joined.Task.LeaseHolder != nil {
		holder = *joined.Task.LeaseHolder
	}
	fmt.Fprintf(w, "goal: %s\n", joined.Task.Goal)
	fmt.Fprintf(w, "status: %s\n", joined.Task.Status)
	fmt.Fprintf(w, "summary: %s\n", joined.Task.Summary)
	fmt.Fprintf(w, "lease_holder: %s\n", holder)
	kinds := make([]string, 0, len(joined.Context))
	for kind := range joined.Context {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(w, "%s:\n", kind)
		for _, ev := range joined.Context[kind] {
			fmt.Fprintf(w, "  [%s] %s\n", ev.ID, ev.Summary)
		}
	}
}

func renderLease(w io.Writer, lease LeaseResponse) {
	holder := ""
	if lease.LeaseHolder != nil {
		holder = *lease.LeaseHolder
	}
	fmt.Fprintf(w, "lease_holder: %s\n", holder)
	fmt.Fprintf(w, "lease_epoch: %d\n", lease.LeaseEpoch)
	if !lease.LeaseExpires.IsZero() {
		fmt.Fprintf(w, "lease_expires: %s\n", lease.LeaseExpires.Format(time.RFC3339))
	}
}

// sessionStatus is the card `sprint session open|status` prints: the derived
// key, the task, the bound sprint, who holds the lease, who has been seen —
// and WHEN the answer was fetched, because a shared view that does not say
// how fresh it is reads as truth.
type sessionStatus struct {
	TaskID       string   `json:"task_id"`
	RepoKey      string   `json:"repo_key,omitempty"`
	RepoRemote   string   `json:"repo_remote,omitempty"`
	SprintSeq    int64    `json:"sprint_seq,omitempty"`
	Created      bool     `json:"created,omitempty"`
	Role         string   `json:"role,omitempty"`
	SeatAsOf     string   `json:"seat_as_of,omitempty"`
	Holder       *string  `json:"lease_holder"`
	Participants []string `json:"participants"`
	Me           string   `json:"me"`
	AsOf         string   `json:"as_of"`
}

func runWeaveSessionStatus(cmd *cobra.Command, flags *weaveOutputFlags) error {
	const verb = "sprint session status"
	mode := flags.mode()
	cwd, _ := os.Getwd()
	sc, err := EnsureRepoSession(cmd.Context(), cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitPrecondFail, err))
	}
	roster, err := sessionRoster(cmd.Context(), sc.client, sc.pointer.TaskID)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, verb, weavecli.ExitGenericFail, err))
	}
	me, _ := SessionParticipant()
	st := sessionStatus{
		TaskID: sc.pointer.TaskID, RepoKey: sc.pointer.RepoKey, RepoRemote: sc.pointer.RepoRemote, SprintSeq: sc.pointer.SprintSeq,
		Created: sc.created, Role: sc.pointer.Role, SeatAsOf: sc.pointer.SeatAsOf, Holder: roster.Holder, Participants: roster.Participants, Me: me,
		AsOf: time.Now().UTC().Format(time.RFC3339),
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, verb, st))
	}
	holder := ""
	if st.Holder != nil {
		holder = *st.Holder
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "session: %s", st.TaskID)
	if st.Created {
		fmt.Fprint(w, "  (created now)")
	}
	fmt.Fprintln(w)
	if st.RepoKey != "" {
		fmt.Fprintf(w, "repo: %s", st.RepoKey)
		if st.RepoRemote != "" && st.RepoRemote != "origin" {
			fmt.Fprintf(w, "  (from remote %s)", st.RepoRemote)
		}
		fmt.Fprintln(w)
	}
	if st.SprintSeq > 0 {
		fmt.Fprintf(w, "sprint: #%d\n", st.SprintSeq)
	}
	fmt.Fprintf(w, "me: %s\n", st.Me)
	if st.Role != "" {
		fmt.Fprintf(w, "role: %s", st.Role)
		if st.SeatAsOf != "" {
			fmt.Fprintf(w, "  (GitHub, as of %s)", st.SeatAsOf)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "lease_holder: %s\n", holder)
	fmt.Fprintln(w, "participants:")
	for _, p := range st.Participants {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintf(w, "as of %s\n", st.AsOf)
	return ec(emitOK(w, mode, verb, nil))
}

func renderRoster(w io.Writer, roster sessionRosterResult) {
	holder := ""
	if roster.Holder != nil {
		holder = *roster.Holder
	}
	fmt.Fprintf(w, "task: %s\n", roster.TaskID)
	fmt.Fprintf(w, "lease_holder: %s\n", holder)
	fmt.Fprintln(w, "participants:")
	for _, p := range roster.Participants {
		fmt.Fprintf(w, "  %s\n", p)
	}
	fmt.Fprintln(w, "events:")
	for _, ev := range roster.Events {
		fmt.Fprintf(w, "  [%s] %s\n", ev.Kind, ev.Summary)
	}
}
