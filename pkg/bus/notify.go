package bus

// `bashy notify` is the send-side front door for one subject-only bus event.
// It adds no message schema and no transport: the subject is Notification.Body,
// the addressee is Notification.To, and Publish remains the enforcement point.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/schedule"
)

// MaxNotifySubjectBytes keeps a notification a doorbell rather than a message
// body. The command refuses overlong subjects; it never silently changes one.
const MaxNotifySubjectBytes = 256

// NotifyReceipt is the machine-readable result of one notify attempt.
type NotifyReceipt struct {
	SchemaVersion string `json:"schema_version"`
	State         string `json:"state"`
	Principal     string `json:"principal,omitempty"`
	To            string `json:"to,omitempty"`
	Subject       string `json:"subject,omitempty"`
	Error         string `json:"error,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	When          string `json:"when,omitempty"`
}

// NotifyEvent is the shared event-side implementation for --notify. Commands
// that already have an event (at, batch, crontab, sleep, timeout) call this
// once when that event occurs; the flag plumbing stays in their common host
// layer instead of five command-specific publishers.
func NotifyEvent(principal, target, subject string) error {
	principal = strings.TrimSpace(principal)
	if principal == "" || principal == "anonymous" {
		return fmt.Errorf("notify: sender identity is required")
	}
	if err := validateNotifySubject(subject); err != nil {
		return err
	}
	addr, _, ok := resolveNotifyTarget(target)
	if !ok {
		return unresolvedTargetError(target)
	}
	return Publish(Notification{Principal: principal, To: addr, Body: subject})
}

// ScheduleNotify records a one-shot notification in the shared scheduler.
// Principal is deliberately captured in the command line stored in the job:
// the scheduler fires outside the submitting process and therefore cannot
// inherit its ambient identity. Replaying an unattributed publish would be
// rejected by bus.Publish at fire time.
func ScheduleNotify(principal, target, subject string, when time.Time) (string, error) {
	principal = strings.TrimSpace(principal)
	target = strings.TrimSpace(target)
	if principal == "" || principal == "anonymous" {
		return "", fmt.Errorf("notify: sender identity is required")
	}
	if target == "" {
		return "", fmt.Errorf("notify: recipient is required")
	}
	if err := validateNotifySubject(subject); err != nil {
		return "", err
	}
	if when.IsZero() || !when.After(time.Now()) {
		return "", fmt.Errorf("notify: scheduled time must be in the future")
	}
	now := time.Now()
	id := strconv.FormatInt(now.UnixNano(), 36)
	// os.Args[0] is the mounted bashy front door. Using argv, rather than a
	// shell string, keeps the subject and captured identity lossless.
	job := &schedule.Job{
		ID: id, Kind: "at", Spec: when.Format(time.RFC3339Nano),
		Command: []string{os.Args[0], "notify", "--as", principal, "--to", target, subject},
		Dir:     os.Getenv("PWD"), Env: append([]string(nil), os.Environ()...), EnvSet: true,
		Enabled: true, CreatedAt: now, NextRun: when,
	}
	if job.Dir == "" {
		job.Dir, _ = os.Getwd()
	}
	if err := schedule.ValidateJobExecution(job); err != nil {
		return "", err
	}
	if err := schedule.StoreFor(job.Dir, job.Env).SubmitJobWithConfirmation(job, func() error { return nil }); err != nil {
		return "", err
	}
	return id, nil
}

// NewNotifyCmd returns the top-level `notify` command.
func NewNotifyCmd() *cobra.Command {
	var as, to, in, at string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "notify [--to <agent>] [<agent>] <subject>",
		Short: "send one subject-only notification to an agent or role",
		Long: `notify sends one subject-only notification through the existing bus.

  bashy notify meridian "meet 3 — your turn"
  bashy notify --to steward "nightly backup failed"

A notification initiates a conversation; it never hosts one. Put the place to
continue in the subject text. Bodies, attachments and a second pointer field are
deliberately absent.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, subject, err := notifyOperands(to, args)
			if err != nil {
				return notifyFailure(cmd, jsonOut, "", target, subject, err)
			}
			if err := validateNotifySubject(subject); err != nil {
				return notifyFailure(cmd, jsonOut, "", target, subject, err)
			}
			principal, err := ResolveAuthoredActor(as)
			if err != nil {
				return notifyFailure(cmd, jsonOut, "", target, subject, err)
			}
			if strings.TrimSpace(principal) == "" || principal == "anonymous" {
				return notifyFailure(cmd, jsonOut, "", target, subject,
					fmt.Errorf("notify: sender identity is required; pass --as or set BASHY_PRINCIPAL"))
			}
			addr, kind, ok := resolveNotifyTarget(target)
			if !ok {
				return notifyFailure(cmd, jsonOut, principal, target, subject, unresolvedTargetError(target))
			}
			if in != "" && at != "" {
				return notifyFailure(cmd, jsonOut, principal, target, subject, fmt.Errorf("notify: --in and --at are mutually exclusive"))
			}
			if in != "" || at != "" {
				when, err := notifyWhen(in, at)
				if err != nil {
					return notifyFailure(cmd, jsonOut, principal, target, subject, err)
				}
				jobID, err := ScheduleNotify(principal, addr, subject, when)
				if err != nil {
					return notifyFailure(cmd, jsonOut, principal, target, subject, err)
				}
				receipt := NotifyReceipt{SchemaVersion: SchemaVersion, State: "scheduled", Principal: principal, To: RoleLabelFor(addr), Subject: subject, JobID: jobID, When: when.Format(time.RFC3339)}
				if jsonOut {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s at %s\n", receipt.State, receipt.To, receipt.When)
				return nil
			}

			before := timelineHigh()
			if err := NotifyEvent(principal, target, subject); err != nil {
				return notifyFailure(cmd, jsonOut, principal, target, subject, err)
			}
			state := StateAccepted
			if kind != TargetRole {
				if seq := findNotificationSeq(before, principal, addr, subject); seq > 0 {
					state = inboxDeliveryState(addr, seq)
				}
			}
			receipt := NotifyReceipt{
				SchemaVersion: SchemaVersion, State: state, Principal: principal,
				To: RoleLabelFor(addr), Subject: subject,
			}
			if jsonOut {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", state, receipt.To)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "send as this identity (default: resolved from your principal)")
	f.StringVar(&to, "to", "", "recipient agent or role (explicit form of the first operand)")
	f.StringVar(&in, "in", "", "deliver after a duration (for example, 15m)")
	f.StringVar(&at, "at", "", "deliver at a local clock time (for example, 09:00)")
	f.BoolVar(&jsonOut, "json", false, "emit a "+SchemaVersion+" delivery receipt")
	return cmd
}

func notifyWhen(in, at string) (time.Time, error) {
	now := time.Now()
	if in != "" {
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("notify: --in must be a positive duration")
		}
		return now.Add(d), nil
	}
	if at == "" {
		return time.Time{}, fmt.Errorf("notify: missing schedule time")
	}
	when, err := schedule.ParseAtTimespecInLocation(at, now, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("notify: invalid --at: %w", err)
	}
	return when, nil
}

func notifyOperands(to string, args []string) (target, subject string, err error) {
	to = strings.TrimSpace(to)
	switch {
	case to == "" && len(args) == 2:
		return strings.TrimSpace(args[0]), args[1], nil
	case to != "" && len(args) == 1:
		return to, args[0], nil
	case to != "" && len(args) == 2:
		return to, args[1], fmt.Errorf("notify: name the addressee once, either as <agent> or with --to")
	case to == "":
		return "", "", fmt.Errorf("notify: requires exactly <agent> and one quoted subject")
	default:
		return to, "", fmt.Errorf("notify: --to requires exactly one quoted subject")
	}
}

func validateNotifySubject(subject string) error {
	if strings.TrimSpace(subject) == "" {
		return fmt.Errorf("notify: subject is required")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("notify: subject must be one line; bodies are not supported")
	}
	if len(subject) > MaxNotifySubjectBytes {
		return fmt.Errorf("notify: subject is %d bytes; maximum is %d (refused, not truncated)", len(subject), MaxNotifySubjectBytes)
	}
	return nil
}

func notifyFailure(cmd *cobra.Command, jsonOut bool, principal, to, subject string, cause error) error {
	if !strings.HasPrefix(strings.ToLower(cause.Error()), StateFailed+":") {
		cause = fmt.Errorf("%s: %w", StateFailed, cause)
	}
	if !jsonOut {
		return cause
	}
	_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(NotifyReceipt{
		SchemaVersion: SchemaVersion, State: StateFailed, Principal: principal,
		To: to, Subject: subject, Error: cause.Error(),
	})
	return reported("%s", cause)
}

// timelineHigh and findNotificationSeq recover the sequence assigned by the
// append-only room store without introducing a second write API. A failure to
// inspect the receipt after a successful Publish leaves the honest lower claim
// `accepted`; it never turns a successful durable write into a reported failure.
func timelineHigh() int64 {
	events, err := watchTimeline(0)
	if err != nil || len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

func findNotificationSeq(after int64, principal, to, subject string) int64 {
	events, err := watchTimeline(0)
	if err != nil {
		return 0
	}
	var seq int64
	for _, e := range events {
		if e.Seq > after && e.Type == "notify" && e.Principal == principal && e.To == to && e.Body == subject {
			seq = e.Seq
		}
	}
	return seq
}

// resolveNotifyTarget starts with the communication family's shared resolver,
// then accepts a name proven by this private channel's own cursor. That final
// case matters for a bus-only subscriber that has never joined the public board:
// its existing inbox is evidence, not a guessed identity.
func resolveNotifyTarget(target string) (addr, kind string, ok bool) {
	if addr, kind, ok := ResolveSendTarget(target); ok {
		return addr, kind, true
	}
	target = strings.TrimSpace(target)
	if _, ok := inboxCursorSeq(target); ok {
		return target, TargetReader, true
	}
	return "", "", false
}

// inboxDeliveryState applies the canonical Delivery vocabulary to the bus
// cursor (not the public board cursor). A missing or malformed cursor proves no
// reader has drained successfully, so the only honest state is unverified.
func inboxDeliveryState(reader string, seq int64) string {
	cur, ok := inboxCursorSeq(reader)
	if !ok {
		return StateUnverified
	}
	if cur >= seq {
		return StateRead
	}
	return StateQueued
}

func inboxCursorSeq(reader string) (int64, bool) {
	path, err := cursorPath(reader)
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	cur, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return cur, true
}
