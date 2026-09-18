package agentpty

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
)

// An agent CLI that stops to ask a question is the quietest way a headless fleet
// fails. It is not slow and it does not crash — it sits at a prompt nobody is
// watching, produces nothing, and eventually trips an idle timeout that reports
// the wrong cause. `agy` does it on every launch ("you are currently not signed
// in"); claude and codex do it on a fresh directory ("do you trust the contents
// of this folder?").
//
// So the PTY tail is classified. A prompt we can safely answer gets answered —
// a trust prompt is cleared with a keystroke. A prompt only a human can answer
// (a device code, a missing API key) is ESCALATED, loudly, rather than guessed
// at. The distinction is the whole design: auto-clearing a trust prompt is
// answering a question the operator already answered by launching the agent
// here; auto-answering an auth challenge would be forging a credential.

// GateKind is the kind of interactive gate an agent appears to be blocked on.
type GateKind string

const (
	GateNone         GateKind = "none"
	GateTrust        GateKind = "trust"
	GateNag          GateKind = "nag"
	GateBrowserOAuth GateKind = "browser_oauth"
	GateDeviceCode   GateKind = "device_code"
	GateAPIKey       GateKind = "api_key"
	GateHuman        GateKind = "human"
)

// GateVerdict is what the tail was classified as.
type GateVerdict struct {
	Kind      GateKind
	URL       string
	Signature string
}

// RouteDeps are the ways out of a gate. Every one is injected, because every one
// is host-specific: how to press a key depends on the control socket, how to open
// a browser depends on the host, and where an escalation GOES depends on whether
// the caller is a work queue or a meeting.
//
// A nil route is not a silent no-op — the routing reports it as unavailable, so
// a gate that cannot be cleared is visible rather than a hang.
type RouteDeps struct {
	Say          func(payload string) error
	BrowserLogin func(url string) error
	Escalate     func(msg string) error
	State        *GateRouteState
	AutoRouteCap int
}

// GateRouteState remembers what has already been routed, so one prompt is
// answered once. It is per-run.
type GateRouteState struct {
	seen       map[string]bool
	autoRoutes int
}

// GateBroker watches a tail that is not moving and decides whether the agent is
// stuck at a gate.
type GateBroker struct {
	deps        RouteDeps
	debounce    time.Duration
	initialized bool
	lastTail    string
	lastChange  time.Time
}

const (
	// DefaultAutoRouteCap bounds how many gates one run may clear by itself.
	// An agent that keeps re-asking is not a prompt to answer harder — it is a
	// loop, and a human should look at it.
	DefaultAutoRouteCap = 3

	// GateTrustClearPayload is the keystroke that answers a trust prompt: the
	// "1" of "1. Yes".
	GateTrustClearPayload = "1"

	// DefaultGateDebounce is how long a tail must sit UNCHANGED before it counts
	// as stuck. An agent mid-answer is not at a gate; only a still screen is.
	DefaultGateDebounce = 2 * time.Second
)

var (
	httpsURLRE        = regexp.MustCompile(`https://[^\s"'<>]+`)
	browserOAuthURLRE = regexp.MustCompile(`(?i)https://[^\s"'<>]*(oauth|authorize|login|callback)[^\s"'<>]*`)
	deviceCodeRE      = regexp.MustCompile(`(?i)\b([A-Z0-9]{4}[- ]?[A-Z0-9]{4}|[A-Z0-9]{6,9})\b`)
)

// authGateSignatures are the phrases an agent emits when it has stopped to ask
// for a human instead of doing work. Matched case-insensitively.
var authGateSignatures = []string{
	"not signed in", "sign in", "sign-in", "please log in", "please login",
	"not logged in", "log in to", "login required", "authentication required",
	"authenticate", "unauthorized", "run `login`", "run 'login'",
	"do you trust", "trust the contents", "trust this", "requires permission",
	"you must log in", "session expired", "no api key", "api key not",
}

// nagGateSignatures are prompts that are not gates at all in spirit — a vendor
// asking for feedback, a "what's new" splash, a rate-us nag — but which block an
// unattended run exactly as hard as an auth gate does, because the TUI is waiting
// on a keypress and does not care that the question is trivial.
//
// Found the hard way: an agy conductor, mid-campaign, stopped dead on
//
//	How's the CLI experience so far?
//	 [1] Good  [2] Fine  [3] Bad  [0] Skip
//
// An agent that is 40 minutes into a task cannot answer a survey, and nothing in
// the fleet knew how to skip it. It would have sat there until the watchdog killed
// it, and the kill would have been reported as a timeout — a wedged run blamed on
// the model, caused by a questionnaire.
var nagGateSignatures = []string{
	"how's the cli experience", "how is the cli experience",
	"rate your experience", "help us improve",
	"[0] skip", "[0] dismiss",
}

// GateNagClearPayload dismisses a nag. "0" is Skip in every menu we have seen;
// answering the survey for the operator would be putting words in their mouth.
const GateNagClearPayload = "0"

// ClassifyGate classifies a live PTY tail into the kind of gate the agent
// appears blocked on. Deliberately pure, so the broker is testable without a
// live tool, a browser, or a terminal.
func ClassifyGate(tail string) GateVerdict {
	low := strings.ToLower(tail)
	if strings.TrimSpace(low) == "" {
		return GateVerdict{Kind: GateNone}
	}
	// Nags first: they are unambiguous, and cheap to be wrong about (a stray "0"
	// into a prompt that was not a nag is a no-op in every TUI here).
	if sig, ok := findSignature(low, nagGateSignatures); ok {
		return GateVerdict{Kind: GateNag, Signature: sig}
	}
	if sig, ok := findSignature(low, []string{
		"do you trust", "trust the contents", "trust this directory",
		"trust this folder", "continue? 1", "1. yes", "1) yes",
		"yes, continue", "yes, proceed",
	}); ok && (strings.Contains(low, "trust") || strings.Contains(low, "continue?")) {
		return GateVerdict{Kind: GateTrust, Signature: sig}
	}
	if sig, ok := findSignature(low, []string{
		"no api key", "api key not set", "api key is not set",
		"missing api key", "api key required",
	}); ok {
		return GateVerdict{Kind: GateAPIKey, Signature: sig}
	}
	if sig, ok := deviceGateSignature(low); ok {
		url := firstDeviceURL(tail)
		return GateVerdict{Kind: GateDeviceCode, URL: url, Signature: sig}
	}
	if sig, ok := authLoginGateSignature(low); ok {
		if url := firstBrowserOAuthURL(tail); url != "" {
			return GateVerdict{Kind: GateBrowserOAuth, URL: url, Signature: sig}
		}
		return GateVerdict{Kind: GateHuman, Signature: sig}
	}
	return GateVerdict{Kind: GateNone}
}

// NewGateBroker builds a broker over the given routes.
func NewGateBroker(deps RouteDeps, debounce time.Duration) *GateBroker {
	if deps.State == nil {
		deps.State = &GateRouteState{}
	}
	if debounce <= 0 {
		debounce = DefaultGateDebounce
	}
	return &GateBroker{deps: deps, debounce: debounce}
}

// ObserveTail feeds the broker the current tail. It routes only when the tail has
// STOPPED CHANGING for the debounce window: an agent halfway through printing a
// trust prompt has not asked yet, and answering a half-drawn question is how you
// send a keystroke into the middle of somebody's sentence.
func (b *GateBroker) ObserveTail(tail string, now time.Time) (GateVerdict, string, error) {
	if b == nil {
		return GateVerdict{Kind: GateNone}, "none", nil
	}
	if !b.initialized {
		b.initialized = true
		b.lastTail = tail
		b.lastChange = now
		return GateVerdict{Kind: GateNone}, "debounce", nil
	}
	if tail != b.lastTail {
		b.lastTail = tail
		b.lastChange = now
		return GateVerdict{Kind: GateNone}, "debounce", nil
	}
	if now.Sub(b.lastChange) < b.debounce {
		return GateVerdict{Kind: GateNone}, "debounce", nil
	}
	verdict := ClassifyGate(tail)
	if verdict.Kind == GateNone {
		return verdict, "none", nil
	}
	action, err := RouteGate(verdict, b.deps)
	return verdict, action, err
}

// RouteGate acts on a verdict, and reports which route it took.
//
// The split that matters: a TRUST gate is cleared automatically, because the
// operator already answered it by launching the agent in this directory. An
// AUTH gate — a device code, a missing key — is escalated to a human, because
// answering it automatically would mean forging a credential. Nothing here ever
// invents an answer to a question it was not asked.
func RouteGate(verdict GateVerdict, deps RouteDeps) (action string, err error) {
	if verdict.Kind == GateNone {
		return "none", nil
	}
	state := deps.State
	if state == nil {
		state = &GateRouteState{}
	}
	if state.seen == nil {
		state.seen = map[string]bool{}
	}
	key := routeDedupeKey(verdict)
	if state.seen[key] {
		return "dedupe", nil
	}
	state.seen[key] = true

	switch verdict.Kind {
	case GateNag:
		// Skip it and get back to work. This is not an authorization decision; it is
		// a questionnaire standing between an agent and its job.
		return "say_nag", callSay(deps, GateNagClearPayload)
	case GateTrust:
		if exceededAutoRouteCap(state, deps.AutoRouteCap) {
			return "escalate", callEscalate(deps, "trust gate auto-route cap reached; human should inspect the worker log before clearing another trust prompt")
		}
		state.autoRoutes++
		return "say_trust", callSay(deps, GateTrustClearPayload)
	case GateBrowserOAuth:
		if exceededAutoRouteCap(state, deps.AutoRouteCap) {
			return "escalate", callEscalate(deps, fmt.Sprintf("browser OAuth gate auto-route cap reached; open %s manually and inspect the worker log", verdict.URL))
		}
		state.autoRoutes++
		if err := callBrowserLogin(deps, verdict.URL); err != nil {
			msg := fmt.Sprintf("browser OAuth login failed for %s: %v; human should complete login or inspect browser availability", verdict.URL, err)
			return "escalate", callEscalate(deps, msg)
		}
		return "browser_login", nil
	case GateDeviceCode:
		msg := "device-code gate detected; open the verification URL"
		if verdict.URL != "" {
			msg += " " + verdict.URL
		}
		msg += " and enter the code shown in the worker log"
		return "escalate", callEscalate(deps, msg)
	case GateAPIKey:
		return "escalate", callEscalate(deps, "API key gate detected; set the missing provider API key and resume or restart the worker")
	case GateHuman:
		msg := "interactive auth gate detected"
		if verdict.Signature != "" {
			msg += " (" + verdict.Signature + ")"
		}
		msg += "; no browser URL or safe keystroke route was found, so a human must inspect the worker log"
		return "escalate", callEscalate(deps, msg)
	default:
		return "none", nil
	}
}

// The control wire lives HERE, and only here.
//
// It used to live in three places: this one, pkg/agentlaunch (its own copy of the
// same twenty lines), and pkg/weave (which hand-rolled its own base64 frames). One
// protocol with three implementations is one protocol that will diverge, and it
// had already begun to: BrokerSay flattens newlines and escapes NULs; weave's
// attach loop did neither, and sent whatever the operator typed straight down the
// socket.
//
// So: agentpty owns the wire — the encoding AND the transport. Everything else
// speaks it through these three functions.

// TextFrame is a line of prose, delivered as typed keystrokes ending in Enter.
//
// Newlines are flattened to spaces, because a newline in the middle of a payload
// would submit half a sentence and leave the rest sitting in the agent's input
// box. A payload containing a NUL cannot survive the line protocol at all, so it
// falls back to the verbatim encoding.
func TextFrame(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
	if strings.ContainsRune(text, '\x00') {
		return VerbatimFrame([]byte(text))
	}
	return text + "\r\n"
}

// VerbatimFrame carries arbitrary BYTES — control characters, Tab, a bare Enter,
// an escape sequence — which a plain line cannot express. This is what `weave say
// --raw/--enter/--tab` needs, and it is why the wire has two frame kinds rather
// than one: "send the agent a sentence" and "press this key at the agent" are
// genuinely different acts.
func VerbatimFrame(payload []byte) string {
	return "\x00R" + base64.StdEncoding.EncodeToString(payload) + "\n"
}

// SendFrame writes a pre-built frame to a run's control channel.
func SendFrame(ctlSock, frame string) error {
	if strings.TrimSpace(ctlSock) == "" {
		return fmt.Errorf("control socket unavailable")
	}
	return sendControlFrame(ctlSock, frame)
}

// BrokerSay writes a line of prose to a run's control socket as keystrokes.
func BrokerSay(ctlSock, payload string) error {
	return SendFrame(ctlSock, TextFrame(payload))
}

// sendControlFrame writes a frame to a run's control channel.
//
// Falls back to a regular file when the socket cannot be dialled — a unix socket
// address caps at ~104 bytes, and a caller with a long path would otherwise lose
// steering entirely rather than merely polling for it.
func sendControlFrame(path, frame string) error {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err == nil {
		defer conn.Close()
		if _, err := conn.Write([]byte(frame)); err != nil {
			return fmt.Errorf("control socket write: %w", err)
		}
		return nil
	}
	if st, statErr := os.Stat(path); statErr == nil && st.Mode().IsRegular() {
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			return fmt.Errorf("control file open: %w", openErr)
		}
		defer f.Close()
		if _, writeErr := f.WriteString(frame); writeErr != nil {
			return fmt.Errorf("control file write: %w", writeErr)
		}
		return nil
	}
	return fmt.Errorf("control socket dial: %w", err)
}

func exceededAutoRouteCap(state *GateRouteState, cap int) bool {
	if cap <= 0 {
		cap = DefaultAutoRouteCap
	}
	return state.autoRoutes >= cap
}

func routeDedupeKey(v GateVerdict) string {
	return string(v.Kind) + "\x00" + v.Signature + "\x00" + v.URL
}

func callSay(deps RouteDeps, payload string) error {
	if deps.Say == nil {
		return fmt.Errorf("say route unavailable")
	}
	return deps.Say(payload)
}

func callBrowserLogin(deps RouteDeps, url string) error {
	if deps.BrowserLogin == nil {
		return fmt.Errorf("browser login route unavailable")
	}
	return deps.BrowserLogin(url)
}

func callEscalate(deps RouteDeps, msg string) error {
	if deps.Escalate == nil {
		return fmt.Errorf("escalation route unavailable: %s", msg)
	}
	return deps.Escalate(msg)
}

func findSignature(low string, signatures []string) (string, bool) {
	for _, sig := range signatures {
		if strings.Contains(low, sig) {
			return sig, true
		}
	}
	return "", false
}

func authLoginGateSignature(low string) (string, bool) {
	signatures := append([]string{}, authGateSignatures...)
	signatures = append(signatures,
		"login to continue",
		"log in to continue",
		"open the following url",
		"open the following link",
		"visit this url",
		"visit the url",
		"complete authentication",
	)
	return findSignature(low, signatures)
}

func deviceGateSignature(low string) (string, bool) {
	deviceWords := []string{"device code", "device login", "verification url", "verification uri", "verify at", "device"}
	codeWords := []string{"enter the code", "use code", "user code", "one-time code", "activation code"}
	deviceSig, hasDevice := findSignature(low, deviceWords)
	codeSig, hasCode := findSignature(low, codeWords)
	if !(hasDevice && hasCode) {
		return "", false
	}
	if firstDeviceURL(low) == "" && !deviceCodeRE.MatchString(low) {
		return "", false
	}
	if codeSig != "" {
		return codeSig, true
	}
	return deviceSig, true
}

func firstBrowserOAuthURL(s string) string {
	return trimURLPunctuation(browserOAuthURLRE.FindString(s))
}

func firstDeviceURL(s string) string {
	for _, u := range httpsURLRE.FindAllString(s, -1) {
		clean := trimURLPunctuation(u)
		low := strings.ToLower(clean)
		if strings.Contains(low, "verify") || strings.Contains(low, "device") || strings.Contains(low, "login") {
			return clean
		}
	}
	return ""
}

func trimURLPunctuation(u string) string {
	return strings.TrimRight(u, ".,);]")
}
