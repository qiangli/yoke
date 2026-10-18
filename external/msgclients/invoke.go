package msgclients

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// Bounds. Defaults are finite; caps are the most any caller may request.
// Every subscription carries all three bounds, so a persistent subscriber
// (an unbounded listener) is inexpressible here: that mode is a long-lived
// service under Rule 1 and is never admitted by this package.
const (
	DefaultMaxMessages = 100
	CapMaxMessages     = 100000
	DefaultMaxBytes    = 1 << 20
	CapMaxBytes        = 16 << 20
	DefaultTimeout     = 30 * time.Second
	CapTimeout         = 10 * time.Minute
	// MaxPayload caps one publish payload (argv never carries it).
	MaxPayload = 1 << 20
	maxStderr  = 64 << 10
)

// Modes.
const (
	ModePublish   = "publish"
	ModeSubscribe = "subscribe"
)

// Op is one bounded operation against an explicit broker. The payload is
// delivered on the child's stdin (mosquitto, kcat) or a 0600 file
// (rabbitmqadmin), never argv. Subscribe always terminates: after
// MaxMessages messages, at MaxBytes of output, or at Timeout — whichever
// comes first — and reports partial explicitly rather than completing
// silently.
type Op struct {
	Mode        string
	Topic       string
	Payload     string
	Exchange    string // rabbitmqadmin publish only ("" = AMQP default exchange)
	QoS         int    // mosquitto only, 0..2
	QoSSet      bool   // whether qos was explicitly selected
	MaxMessages int
	MaxBytes    int64
	Timeout     time.Duration
}

// Invocation is a fully built, not-yet-run child: argv without secrets,
// environment additions (secrets travel here), the stdin payload, and
// auxiliary file contents materialized as 0600 temp files at Run.
type Invocation struct {
	Client string
	Bin    string
	Args   []string
	Env    []string // additions; ambient messaging variables are scrubbed first
	Stdin  string
	Op     Op
	Secret string // for redaction only
	// Aux maps a sentinel token present in Args to the file content Run
	// writes for it. Sentinels never reach the child; Explain renders them
	// as ‹tempfile› without content.
	Aux map[string]string
}

// Aux sentinel tokens (appear in Args; replaced with temp paths at Run).
const (
	auxKcatConfig   = "@AUX/kcat-config"
	auxPayloadFile  = "@AUX/payload-file"
	auxDisplay      = "‹tempfile›"
	payloadExplain  = 1024 // explain truncates the payload preview here
	redactedDisplay = "‹redacted›"
)

// ambientPrefixes are messaging variables scrubbed from the inherited env
// so a shell-level MQTT_*/RABBITMQ_*/KAFKA_* variable can never override
// or supply the explicit selection.
var ambientPrefixes = map[string][]string{
	"mosquitto":     {"MQTT", "MOSQUITTO"},
	"rabbitmqadmin": {"RABBITMQ"},
	"kcat":          {"KAFKA", "KCAT", "RD_KAFKA"},
}

// withDefaults applies finite bounds without validating the operation,
// so readers (Parse) stay bounded even for values Build would refuse.
func (o Op) withDefaults() Op {
	if o.MaxMessages == 0 {
		o.MaxMessages = DefaultMaxMessages
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Timeout == 0 {
		o.Timeout = DefaultTimeout
	}
	return o
}

func (o Op) normalized() (Op, *gws.Error) {
	if o.Mode != ModePublish && o.Mode != ModeSubscribe {
		return o, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("messaging: mode %q unknown (want %s or %s) — the effect must be explicit", o.Mode, ModePublish, ModeSubscribe), nil)
	}
	if strings.TrimSpace(o.Topic) == "" {
		return o, gws.NewError(gws.CodeInvalidInput, "messaging: empty topic — missing required input fails promptly", nil)
	}
	if o.MaxMessages < 0 || o.MaxMessages > CapMaxMessages || o.MaxBytes < 0 || o.MaxBytes > CapMaxBytes || o.Timeout < 0 || o.Timeout > CapTimeout {
		return o, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("messaging: bounds over cap (messages<=%d bytes<=%d timeout<=%s)", CapMaxMessages, CapMaxBytes, CapTimeout), nil)
	}
	return o.withDefaults(), nil
}

// Build validates the whole operation locally (no network, no effects) and
// returns the invocation. Unsupported combinations are refused here, before
// any provider execution, identically for CLI and MCP callers.
func Build(ep Endpoint, op Op, bin string) (Invocation, *gws.Error) {
	c, ok := Lookup(ep.Client)
	if !ok {
		return Invocation{}, gws.NewError(gws.CodeUnsupported, ep.Client+": not an admitted messaging client", nil)
	}
	op, err := op.normalized()
	if err != nil {
		return Invocation{}, err.WithOp(op.Mode, ep.Client)
	}
	inv := Invocation{Client: ep.Client, Bin: bin, Op: op, Secret: ep.Password, Aux: map[string]string{}}
	var e *gws.Error
	switch ep.Client {
	case "mosquitto":
		e = buildMosquitto(&inv, ep, op)
	case "rabbitmqadmin":
		e = buildRabbitMQ(&inv, ep, op)
	case "kcat":
		e = buildKcat(&inv, ep, op)
	}
	if e != nil {
		return Invocation{}, e.WithOp(op.Mode, c.Name)
	}
	return inv, nil
}

// checkCommon refuses option/client combinations that do not exist.
func checkCommon(client string, op Op) *gws.Error {
	if op.QoSSet && client != "mosquitto" {
		return gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: qos is an MQTT option and is refused here", client), nil)
	}
	if op.QoSSet && (op.QoS < 0 || op.QoS > 2) {
		return gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("mosquitto: qos %d out of range (want 0..2)", op.QoS), nil)
	}
	if op.Exchange != "" && client != "rabbitmqadmin" {
		return gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: exchange is a RabbitMQ publish option and is refused here", client), nil)
	}
	if op.Mode == ModeSubscribe {
		if op.Payload != "" {
			return gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: subscribe takes no payload — select mode publish to send", client), nil)
		}
		if op.Exchange != "" {
			return gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: subscribe takes no exchange — it consumes the named queue", client), nil)
		}
	} else {
		if int64(len(op.Payload)) > MaxPayload {
			return gws.NewError(gws.CodeOutputLimit, fmt.Sprintf("%s: payload exceeds the %d-byte input limit", client, MaxPayload), nil)
		}
	}
	return nil
}

// mosquittoTLS appends the TLS selection. TLS verify requires an explicit
// CA file: the client enables TLS via --cafile, and falling back to an
// unwired system store would risk a silent plaintext downgrade, so the
// missing file is a loud refusal, not a default.
func mosquittoTLS(args []string, ep Endpoint) ([]string, *gws.Error) {
	if ep.TLS != TLSVerify {
		return args, nil
	}
	if ep.CAFile == "" {
		return nil, gws.NewError(gws.CodeInvalidInput, fmt.Sprintf("%s: TLS verify needs an explicit %s_CA_FILE — refusing to guess a trust store", ep.Client, LookupPrefix(ep.Client)), nil)
	}
	return append(args, "--cafile", ep.CAFile), nil
}

func buildMosquitto(inv *Invocation, ep Endpoint, op Op) *gws.Error {
	if e := checkCommon("mosquitto", op); e != nil {
		return e
	}
	qos := op.QoS
	args := []string{"-h", ep.Host, "-p", strconv.Itoa(ep.Port), "-t", op.Topic, "-q", strconv.Itoa(qos)}
	var e *gws.Error
	if args, e = mosquittoTLS(args, ep); e != nil {
		return e
	}
	if ep.User != "" {
		args = append(args, "-u", ep.User)
	}
	switch op.Mode {
	case ModePublish:
		// -s sends the whole of stdin as one message, so a structured
		// payload (multi-line JSON included) survives intact. The
		// password travels as the first stdin line, consumed as the
		// interactive password prompt (unverified against the real
		// binary — see EVIDENCE.md); it is never argv.
		args = append(args, "-s")
		if ep.User != "" {
			inv.Stdin = ep.Password + "\n" + op.Payload
		} else {
			inv.Stdin = op.Payload
		}
	case ModeSubscribe:
		// -C bounds the subscription server-side (disconnect after N
		// messages); -F frames topic and payload for exact parsing.
		// The wrapper deadline and byte/row limits bound it again.
		args = append(args, "-C", strconv.Itoa(op.MaxMessages), "-F", "%t\x1f%p\x1e")
		if ep.User != "" {
			inv.Stdin = ep.Password + "\n"
		}
	}
	inv.Args = args
	return nil
}

func rabbitServer(ep Endpoint) string {
	scheme := "https"
	if ep.TLS != TLSVerify {
		scheme = "http"
	}
	host := ep.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, ep.Port)
}

func buildRabbitMQ(inv *Invocation, ep Endpoint, op Op) *gws.Error {
	if e := checkCommon("rabbitmqadmin", op); e != nil {
		return e
	}
	base := []string{"--server", rabbitServer(ep), "--username", ep.User}
	switch op.Mode {
	case ModePublish:
		// The payload travels in a 0600 file materialized at Run, never
		// argv: payloads may carry secrets and newlines. Flag spellings
		// are unverified against the real v2 binary (see EVIDENCE.md);
		// an unknown flag fails loudly, never silently.
		inv.Aux[auxPayloadFile] = op.Payload
		args := append([]string(nil), base...)
		args = append(args, "publish", "--exchange", op.Exchange, "--routing-key", op.Topic, "--payload-file", auxPayloadFile)
		inv.Args = args
	case ModeSubscribe:
		// get consumes from the named queue, bounded by --count; the
		// ackmode requeues so a bounded peek is not a destructive read.
		// The wrapper deadline and byte/row limits bound it again.
		args := append([]string(nil), base...)
		args = append(args, "get", "--queue", op.Topic, "--count", strconv.Itoa(op.MaxMessages), "--ackmode", "ack_requeue_true", "--format", "json")
		inv.Args = args
	}
	// Credential handoff: server and username are argv (psql -U precedent);
	// the password travels by env only. Whether the v2 binary honors this
	// variable is unverified (see EVIDENCE.md).
	inv.Env = []string{"RABBITMQADMIN_PASSWORD=" + ep.Password}
	if ep.TLS == TLSVerify && ep.CAFile != "" {
		inv.Env = append(inv.Env, "RABBITMQADMIN_CAFILE="+ep.CAFile)
	}
	return nil
}

// kcatConfig renders the librdkafka configuration for -F. Secrets stay in
// this file (0600 at Run), never argv.
func kcatConfig(ep Endpoint) string {
	var lines []string
	proto := "PLAINTEXT"
	if ep.TLS == TLSVerify {
		proto = "SSL"
	}
	if ep.User != "" {
		if ep.TLS == TLSVerify {
			proto = "SASL_SSL"
		} else {
			proto = "SASL_PLAINTEXT"
		}
	}
	lines = append(lines, "security.protocol="+proto)
	if ep.TLS == TLSVerify && ep.CAFile != "" {
		lines = append(lines, "ssl.ca.location="+ep.CAFile)
	}
	if ep.User != "" {
		lines = append(lines, "sasl.mechanisms=PLAIN", "sasl.username="+ep.User, "sasl.password="+ep.Password)
	}
	return strings.Join(lines, "\n") + "\n"
}

func buildKcat(inv *Invocation, ep Endpoint, op Op) *gws.Error {
	if e := checkCommon("kcat", op); e != nil {
		return e
	}
	broker := ep.Host + ":" + strconv.Itoa(ep.Port)
	if strings.Contains(ep.Host, ":") && !strings.HasPrefix(ep.Host, "[") {
		broker = "[" + ep.Host + "]:" + strconv.Itoa(ep.Port)
	}
	args := []string{"-b", broker, "-t", op.Topic}
	if cfg := kcatConfig(ep); cfg != "security.protocol=PLAINTEXT\n" {
		inv.Aux[auxKcatConfig] = cfg
		args = append(args, "-F", auxKcatConfig)
	}
	switch op.Mode {
	case ModePublish:
		// -P produces from stdin; -D splits stdin on the record
		// separator so one structured payload stays one message.
		args = append(args, "-P", "-D", "\x1e")
		inv.Stdin = op.Payload
	case ModeSubscribe:
		// -J emits one JSON object per message for exact parsing; -c
		// caps the consume server-side; the wrapper deadline and
		// byte/row limits bound it again.
		args = append(args, "-C", "-o", "beginning", "-e", "-c", strconv.Itoa(op.MaxMessages), "-J")
	}
	inv.Args = args
	return nil
}

// LookupPrefix reports the env prefix for diagnostics.
func LookupPrefix(client string) string {
	if c, ok := Lookup(client); ok {
		return c.Prefix
	}
	return strings.ToUpper(client)
}

// Explain describes what would run without executing: argv (secret-masked,
// temp-file sentinels shown as ‹tempfile›), the redacted scope, and the
// declared effects. It never touches the network. It is a local preview,
// not execution evidence.
func (inv Invocation) Explain(ep Endpoint) map[string]any {
	argv, _ := gws.RedactArgs(renderArgs(inv.Args), inv.Secret)
	mut, effects := "none (bounded read; no broker write)", "net.egress("+ep.Host+") child.exec("+inv.Bin+")"
	if inv.Op.Mode == ModePublish {
		mut = "PUBLISHED on success (explicit broker write to " + inv.Op.Topic + ")"
		effects += " broker.publish(" + inv.Op.Topic + ")"
	} else {
		effects += " broker.consume-bounded(" + inv.Op.Topic + ",<=" + strconv.Itoa(inv.Op.MaxMessages) + ")"
	}
	payload := gws.RedactText(inv.Op.Payload, inv.Secret)
	if len(payload) > payloadExplain {
		payload = payload[:payloadExplain] + "…"
	}
	return map[string]any{
		"preview":  "local",
		"client":   inv.Client,
		"mode":     inv.Op.Mode,
		"argv":     argv,
		"scope":    ep.RedactedScope(),
		"mutation": mut,
		"effects":  effects,
		"bounds":   map[string]any{"max_messages": inv.Op.MaxMessages, "max_bytes": inv.Op.MaxBytes, "timeout": inv.Op.Timeout.String()},
		"fidelity": clients[inv.Client].Fidelity,
		"topic":    gws.RedactText(inv.Op.Topic, inv.Secret),
		"payload":  payload,
	}
}

// renderArgs swaps aux sentinels for a stable display token before
// redaction, so Explain never leaks a temp path or file content.
func renderArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == auxKcatConfig || a == auxPayloadFile {
			out[i] = auxDisplay
		} else {
			out[i] = a
		}
	}
	return out
}
