package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// MaxHistoryPages bounds the provider pagination walked inside one Gmail
// history Poll. Exhausting it is not completion: the batch's cursor stays
// at the last fully walked position and the next Poll continues.
const MaxHistoryPages = 10

// GmailHistorySource polls users.history.list through the registered gws
// command. The change cursor is the Gmail historyId; the provider's
// nextPageToken is pagination inside one Poll and is never stored as the
// cursor. An expired historyId (HTTP 404) is a typed cursor-expired error.
type GmailHistorySource struct {
	Run Runner
	// User is the explicit mailbox ("me" only when the selected credential
	// resolves it unambiguously); no ambient default.
	User string
}

type gmailHistoryPage struct {
	History []struct {
		ID string `json:"id"`
	} `json:"history"`
	NextPageToken string `json:"nextPageToken"`
	HistoryID     string `json:"historyId"`
}

// Poll implements Source.
func (g GmailHistorySource) Poll(ctx context.Context, cursor string) (Batch, error) {
	if g.User == "" {
		return Batch{}, gws.NewError(gws.CodeInvalidInput, "gws events: gmail history needs an explicit user", nil)
	}
	var batch Batch
	pageToken := ""
	for page := 0; page < MaxHistoryPages; page++ {
		params := map[string]string{"userId": g.User, "startHistoryId": cursor}
		if pageToken != "" {
			params["pageToken"] = pageToken
		}
		p, _ := json.Marshal(params)
		out, err := g.Run(ctx, []string{"gws", "gmail", "users", "history", "list", "--params", string(p)})
		if err != nil {
			return Batch{}, classify(out, err, "gmail history")
		}
		var hp gmailHistoryPage
		if jerr := json.Unmarshal(out, &hp); jerr != nil {
			return Batch{}, gws.NewError(gws.CodeProviderFailure, "gws events: unparseable gmail history response", nil)
		}
		raw := struct {
			History []json.RawMessage `json:"history"`
		}{}
		_ = json.Unmarshal(out, &raw)
		for i, h := range hp.History {
			batch.Events = append(batch.Events, Event{ID: h.ID, Kind: "gmail.history", Payload: raw.History[i]})
		}
		if hp.NextPageToken == "" {
			batch.Next = hp.HistoryID
			return batch, nil
		}
		pageToken = hp.NextPageToken
	}
	// Bound hit mid-walk: only hand back what is safe to acknowledge. The
	// cursor of the last full history record is the resume position.
	if n := len(batch.Events); n > 0 {
		batch.Next = batch.Events[n-1].ID
	} else {
		batch.Next = cursor
	}
	return batch, nil
}

// classify maps a failed gws invocation onto the shared typed codes. The
// provider error body is parsed for its code only; the caller redacts the
// message before it leaves the adapter.
func classify(out []byte, runErr error, op string) error {
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(out, &body)
	rateLimited := body.Error.Status == "RESOURCE_EXHAUSTED"
	for _, e := range body.Error.Errors {
		if e.Reason == "rateLimitExceeded" || e.Reason == "userRateLimitExceeded" {
			rateLimited = true
		}
	}
	pc := body.Error.Code
	detail := &gws.Error{ProviderCode: strconv.Itoa(pc)}
	msg := fmt.Sprintf("gws events: %s failed (provider %d %s): %s", op, pc, body.Error.Status, body.Error.Message)
	if pc == 0 {
		detail.ProviderCode = ""
		msg = fmt.Sprintf("gws events: %s failed: %v", op, runErr)
	}
	var code gws.Code
	switch {
	case pc == 404 || pc == 410:
		code = gws.CodeCursorExpired
		msg = fmt.Sprintf("gws events: %s cursor expired at the provider — choose an explicit Resync (new watch historyId or full sync); the checkpoint was not reset", op)
	case pc == 401:
		code = gws.CodeExpiredCredentials
	case pc == 429, pc == 403 && rateLimited:
		code, detail.Retryable = gws.CodeRateLimit, true
	case pc == 403:
		code = gws.CodePermissionDenied
	case pc >= 500 || pc == 0:
		code, detail.Retryable = gws.CodeProviderFailure, true
	default:
		code = gws.CodeProviderFailure
	}
	return gws.NewError(code, msg, detail).WithOp(op, "")
}

// GmailWatchArgv is the registered-command argv that (re)registers a Gmail
// watch on a Pub/Sub topic. Re-watching is how a Gmail watch lease renews.
func GmailWatchArgv(user, topic string) []string {
	p, _ := json.Marshal(map[string]string{"userId": user})
	b, _ := json.Marshal(map[string]string{"topicName": topic})
	return []string{"gws", "gmail", "users", "watch", "--params", string(p), "--json", string(b)}
}

// ParseGmailWatch reads a users.watch response: the starting historyId and
// the lease expiry (epoch milliseconds).
func ParseGmailWatch(out []byte) (historyID string, expires time.Time, err error) {
	var r struct {
		HistoryID  string `json:"historyId"`
		Expiration string `json:"expiration"`
	}
	if jerr := json.Unmarshal(out, &r); jerr != nil || r.HistoryID == "" || r.Expiration == "" {
		return "", time.Time{}, gws.NewError(gws.CodeProviderFailure, "gws events: watch response lacks historyId/expiration — lease state unknown", nil)
	}
	ms, perr := strconv.ParseInt(r.Expiration, 10, 64)
	if perr != nil {
		return "", time.Time{}, gws.NewError(gws.CodeProviderFailure, "gws events: watch expiration is not epoch milliseconds", nil)
	}
	return r.HistoryID, time.UnixMilli(ms).UTC(), nil
}

// EventsRenewArgv is the registered-command argv that renews a Workspace
// Events subscription by patching its ttl ("0s" asks for the maximum).
func EventsRenewArgv(name string) []string {
	p, _ := json.Marshal(map[string]string{"name": name, "updateMask": "ttl"})
	b, _ := json.Marshal(map[string]string{"ttl": "0s"})
	return []string{"gws", "events", "subscriptions", "patch", "--params", string(p), "--json", string(b)}
}

// ParseEventsRenew reads the subscriptions.patch long-running operation and
// returns the new expireTime. An operation that is not done yet leaves the
// renewal outcome unknown — reported as such, never assumed successful.
func ParseEventsRenew(out []byte) (time.Time, error) {
	var op struct {
		Done     bool `json:"done"`
		Response struct {
			ExpireTime string `json:"expireTime"`
		} `json:"response"`
	}
	if err := json.Unmarshal(out, &op); err != nil {
		return time.Time{}, gws.NewError(gws.CodeProviderFailure, "gws events: unparseable subscription renew response", nil)
	}
	if !op.Done || op.Response.ExpireTime == "" {
		return time.Time{}, gws.NewError(gws.CodePartialMutation, "gws events: subscription renewal accepted but not confirmed — re-check before relying on the lease", nil)
	}
	t, err := time.Parse(time.RFC3339Nano, op.Response.ExpireTime)
	if err != nil {
		return time.Time{}, gws.NewError(gws.CodeProviderFailure, "gws events: subscription expireTime is not RFC 3339", nil)
	}
	return t.UTC(), nil
}
