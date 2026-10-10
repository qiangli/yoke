package fleet

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// Asking the model door for a context window (F17). A tool's
// cli.launch.min_context does not have to refuse a model whose DECLARED
// window is smaller when the door serves that model itself: the launch asks,
// and the ask rides the door's own sticky-binding path — the URL form
// `bashy llm env --sticky` prints — because that is the one carrier a
// base-URL-only client (every fleet tool) already speaks. The binding is
// created with num_ctx, the same call `bashy llm sticky create KEY --model M
// --num-ctx N` makes, and every request under the key is served with at
// least that context. Rendering is pure; the launcher creates the binding.

// IsDoorBaseURL reports whether baseURL points at the host's model door: the
// loopback, on the door's port (24556 or $BASHY_LLM_PORT).
func IsDoorBaseURL(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		return false
	}
	return u.Port() == strconv.Itoa(door.Port())
}

// DoorContextBinding renders the base URL a launch uses to ask the door for
// at least minCtx tokens of context for model: the original door URL with a
// context-asking sticky binding wrapped around it, keyed deterministically by
// model and size (creation is idempotent, so every launch of the same
// binding reuses it).
//
// ok is false when baseURL is not a door URL, minCtx is not positive, no
// model is bound, or the URL already rides a /sticky/<key> path — a frozen
// identity whose context the door cannot raise, so F16's up-front refusal
// stays the authority there.
func DoorContextBinding(baseURL, model string, minCtx int64) (key, rewritten string, ok bool) {
	if minCtx <= 0 || strings.TrimSpace(model) == "" || !IsDoorBaseURL(baseURL) {
		return "", "", false
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", "", false
	}
	// Peel the door's path parameters (/k/<token>, /s/<session>,
	// /sticky/<key>) the way the door itself does, so the binding is
	// inserted ahead of the endpoint path and after anything already there.
	var prefix []string
	p := u.Path
	for {
		trimmed := strings.TrimPrefix(p, "/")
		name, rest, found := strings.Cut(trimmed, "/")
		if !found || (name != "k" && name != "s" && name != "sticky") {
			break
		}
		if name == "sticky" {
			return "", "", false // a frozen identity: not ours to raise
		}
		val, after, _ := strings.Cut(rest, "/")
		if v, err := url.PathUnescape(val); err == nil {
			val = v
		}
		prefix = append(prefix, name, val)
		p = "/" + after
	}
	key = doorContextKey(model, minCtx)
	out := u.Scheme + "://" + u.Host
	for _, seg := range prefix {
		out += "/" + seg
	}
	return key, out + "/sticky/" + key + p, true
}

// doorContextKey names the binding after what it asks for. Sticky keys may
// hold letters, digits, - _ and . only; a model spelling outside that folds
// in, so the key stays one per model-and-size.
func doorContextKey(model string, minCtx int64) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, model)
	return "minctx." + safe + "." + strconv.FormatInt(minCtx, 10)
}
