package events

import (
	"context"
	"fmt"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"

	"github.com/qiangli/yoke/external/gws"
)

// Lease lifetimes and the renewal cadence derived from them.
const (
	// GmailWatchMaxLifetime: a Gmail watch expires within 7 days.
	GmailWatchMaxLifetime = 7 * 24 * time.Hour
	// GmailWatchRenewEvery: Google recommends re-watching once a day.
	GmailWatchRenewEvery = 24 * time.Hour
	// DefaultRenewMargin renews a lease this long before it lapses.
	DefaultRenewMargin = 12 * time.Hour
)

// RenewFunc renews one lease and returns its new expiry.
type RenewFunc func(ctx context.Context, l Lease) (time.Time, error)

// Renewer renews the Store's leases that fall due within Margin. One
// RenewDue call is bounded; run it from the existing scheduler.
type Renewer struct {
	Renew   RenewFunc
	Margin  time.Duration
	Backoff Backoff
	Now     func() time.Time
	Sleep   Sleep
	Secrets []string
}

// RenewDue renews every due lease, persisting each new expiry before moving
// on. A lease that already lapsed is NOT renewed silently: notifications may
// have been missed, so it fails typed (cursor-expired) and the operator
// resubscribes and polls history from the checkpoint. A failed renewal
// leaves the stored expiry unchanged.
func (r Renewer) RenewDue(ctx context.Context, s *Store) ([]Lease, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	margin := r.Margin
	if margin <= 0 {
		margin = DefaultRenewMargin
	}
	backoff := r.Backoff
	if backoff.Attempts == 0 {
		backoff = DefaultBackoff
	}
	sleep := r.Sleep
	if sleep == nil {
		sleep = ctxSleep
	}
	var renewed []Lease
	for _, l := range s.Checkpoint().Leases {
		t := now()
		if !t.Before(l.Expires) {
			return renewed, gws.NewError(gws.CodeCursorExpired, fmt.Sprintf("gws events: %s lease %q lapsed at %s — resubscribe explicitly, then poll from the checkpoint", l.Kind, l.Name, l.Expires.Format(time.RFC3339)), nil).WithOp("renew", l.Name)
		}
		if l.Expires.Sub(t) > margin {
			continue
		}
		var exp time.Time
		err := retry(ctx, backoff, sleep, func() error {
			var rerr error
			exp, rerr = r.Renew(ctx, l)
			return rerr
		})
		if err != nil {
			return renewed, redactErr(err, l.Name, r.Secrets)
		}
		if !exp.After(l.Expires) {
			return renewed, gws.NewError(gws.CodeProviderFailure, fmt.Sprintf("gws events: renewal of %q did not extend the lease", l.Name), nil).WithOp("renew", l.Name)
		}
		l.Expires = exp
		if err := s.PutLease(l); err != nil {
			return renewed, err
		}
		renewed = append(renewed, l)
	}
	return renewed, nil
}

// CommandRenewer renews leases through the registered gws command: Gmail
// watches by re-watching topic, Events subscriptions by patching their ttl.
func CommandRenewer(run Runner, user, topic string) RenewFunc {
	return func(ctx context.Context, l Lease) (time.Time, error) {
		switch l.Kind {
		case LeaseGmailWatch:
			out, err := run(ctx, GmailWatchArgv(user, topic))
			if err != nil {
				return time.Time{}, classify(out, err, "gmail watch")
			}
			_, exp, perr := ParseGmailWatch(out)
			return exp, perr
		case LeaseEventsSubscription:
			out, err := run(ctx, EventsRenewArgv(l.Name))
			if err != nil {
				return time.Time{}, classify(out, err, "events renew")
			}
			return ParseEventsRenew(out)
		}
		return time.Time{}, gws.Unsupported("renew", l.Kind)
	}
}

// RenewalJob builds an existing-scheduler job (pkg/schedule, kind "every")
// that runs the registered-command argv on a cadence. It refuses a cadence
// that could let the lease lapse: every + margin must fit inside lifetime.
func RenewalJob(id string, every, lifetime time.Duration, argv []string) (*schedule.Job, error) {
	if id == "" || len(argv) == 0 {
		return nil, gws.NewError(gws.CodeInvalidInput, "gws events: renewal job needs an id and a command", nil)
	}
	if every <= 0 || every+DefaultRenewMargin > lifetime {
		return nil, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("gws events: renewal every %s cannot keep a %s lease alive with a %s margin", every, lifetime, DefaultRenewMargin), nil)
	}
	return &schedule.Job{
		ID: id, Name: "gws-events-renew " + id, Kind: "every", Spec: every.String(),
		Command: append([]string(nil), argv...), Enabled: true, CreatedAt: time.Now().UTC(),
	}, nil
}
