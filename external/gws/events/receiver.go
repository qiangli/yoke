package events

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/qiangli/yoke/external/gws"
)

// MaxPushBody bounds one Pub/Sub push request body.
const MaxPushBody = 1 << 20

// Authenticator decides whether a push request comes from the configured
// sender. It must not echo presented or expected credentials.
type Authenticator func(r *http.Request) bool

// SharedToken authenticates a push endpoint configured with a secret token
// (Pub/Sub push "?token=" endpoint parameter, or the X-Goog-Channel-Token
// set on a watch channel), compared in constant time. An empty secret
// authenticates nothing.
func SharedToken(secret string) Authenticator {
	return func(r *http.Request) bool {
		if secret == "" {
			return false
		}
		got := r.URL.Query().Get("token")
		if got == "" {
			got = r.Header.Get("X-Goog-Channel-Token")
		}
		return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
	}
}

// BearerVerifier authenticates a Pub/Sub authenticated push: the OIDC JWT
// in "Authorization: Bearer" is handed to verify, which the caller supplies
// (signature against Google's keys, audience and service-account email).
// This package ships no JWKS fetcher; a nil verify authenticates nothing.
func BearerVerifier(verify func(ctx context.Context, jwt string) error) Authenticator {
	return func(r *http.Request) bool {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" || verify == nil {
			return false
		}
		return verify(r.Context(), tok) == nil
	}
}

// Receiver is the Pub/Sub push handler for Gmail watch and Workspace Events
// notifications. It authenticates, bounds and decodes the push, dedups by
// event id, runs the Handler, and acknowledges (records the id, then
// answers 2xx) only after the Handler succeeded; any failure answers
// non-2xx so Pub/Sub redelivers (at-least-once). Deliveries are processed
// one at a time, in arrival order.
//
// Receiver is an http.Handler only — this package never listens. Hosting
// it is a long-lived managed service: see ServeAdmission.
type Receiver struct {
	Auth    Authenticator
	Store   *Store
	Handler Handler

	mu sync.Mutex
}

type pushEnvelope struct {
	Message struct {
		Data       string            `json:"data"`
		MessageID  string            `json:"messageId"`
		Attributes map[string]string `json:"attributes"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// ServeHTTP implements http.Handler.
func (rv *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rv.Auth == nil || !rv.Auth(r) {
		http.Error(w, "unauthenticated push", http.StatusUnauthorized)
		return
	}
	body, berr := gws.ReadBounded(r.Body, MaxPushBody)
	if berr != nil {
		http.Error(w, "push body exceeds limit", http.StatusRequestEntityTooLarge)
		return
	}
	var env pushEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Message.MessageID == "" {
		http.Error(w, "malformed push envelope", http.StatusBadRequest)
		return
	}
	data, err := base64.StdEncoding.DecodeString(env.Message.Data)
	if err != nil || (len(data) > 0 && !json.Valid(data)) {
		http.Error(w, "malformed push data", http.StatusBadRequest)
		return
	}
	ev := Event{ID: env.Message.MessageID, Kind: "pubsub.push", Payload: data}
	// Workspace Events deliveries are CloudEvents: ce-id is the stable id
	// across Pub/Sub redeliveries of the same event.
	if id := env.Message.Attributes["ce-id"]; id != "" {
		ev.ID, ev.Kind = id, env.Message.Attributes["ce-type"]
	}
	if len(ev.Payload) == 0 {
		ev.Payload = nil
	}

	rv.mu.Lock()
	defer rv.mu.Unlock()
	if rv.Store.Seen(ev.ID) {
		w.WriteHeader(http.StatusNoContent) // duplicate: already processed, ack again
		return
	}
	if err := rv.Handler(r.Context(), ev); err != nil {
		http.Error(w, "handler failed; event not acknowledged", http.StatusServiceUnavailable)
		return
	}
	if err := rv.Store.Ack(ev.ID); err != nil {
		http.Error(w, "checkpoint write failed; event not acknowledged", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeAdmission reports whether this build may HOST a push receiver. A
// receiver is a long-lived service under service Rule 1 (permissive,
// Bashy-built, signed, no-egress verified, local/LAN bind, remote access
// only through outpost + TLS). That evidence does not exist for this
// adapter yet, so hosting is disabled with an actionable reason; polling
// (RunOnce) and renewal (RenewDue) remain available as bounded scheduled
// invocations.
func ServeAdmission() error {
	return gws.NewError(gws.CodeUnsupported, "gws events: hosting a push receiver is a managed long-lived service (service Rule 1) — disabled until a Bashy-signed build with no-egress evidence exists; use scheduled polling (RunOnce) meanwhile", nil).WithOp("serve", "receiver")
}
