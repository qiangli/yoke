package msgclients

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

const canary = "CANARY-s3cr3t-Zx9qLm42"

// TestHelperProcess is the stand-in client: it replays a recorded output
// fixture (never a live broker) so the real exec/cancel/bound paths run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MC_HELPER") != "1" {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	if p := os.Getenv("MC_STDIN_OUT"); p != "" {
		_ = os.WriteFile(p, in, 0o600)
		_ = os.WriteFile(p+".env", []byte(strings.Join(os.Environ(), "\n")), 0o600)
		argv := []byte(strings.Join(os.Args, "\n"))
		_ = os.WriteFile(p+".argv", argv, 0o600)
	}
	sub := strings.NewReplacer("<US>", "\x1f", "<RS>", "\x1e")
	switch os.Getenv("MC_MODE") {
	case "sleep":
		time.Sleep(time.Minute)
	case "flood":
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for {
			if _, err := os.Stdout.Write(chunk); err != nil {
				os.Exit(0)
			}
		}
	case "fail":
		b, _ := os.ReadFile(os.Getenv("MC_FIXTURE"))
		fmt.Fprint(os.Stderr, strings.ReplaceAll(string(b), "CANARY-PLACEHOLDER", os.Getenv("MC_CANARY")))
		os.Exit(2)
	default:
		b, _ := os.ReadFile(os.Getenv("MC_FIXTURE"))
		fmt.Fprint(os.Stdout, sub.Replace(string(b)))
	}
	os.Exit(0)
}

func endpoint(client string) Endpoint {
	switch client {
	case "rabbitmqadmin":
		return Endpoint{Client: client, Host: "mq.example.test", Port: 15671, User: "app", TLS: TLSVerify, CAFile: "/etc/ca.pem", Password: canary}
	default:
		return Endpoint{Client: client, Host: "mq.example.test", Port: 8883, User: "app", TLS: TLSVerify, CAFile: "/etc/ca.pem", Password: canary}
	}
}

func helperInv(t *testing.T, client string, op Op, fixture, mode string) Invocation {
	t.Helper()
	inv, err := Build(endpoint(client), op, os.Args[0])
	if err != nil {
		t.Fatalf("Build(%s): %v", client, err)
	}
	inv.Args = []string{"-test.run=^TestHelperProcess$"}
	dir := t.TempDir()
	inv.Env = append(inv.Env, "MC_HELPER=1", "MC_MODE="+mode, "MC_FIXTURE="+filepath.Join("testdata", fixture),
		"MC_STDIN_OUT="+filepath.Join(dir, "stdin"), "MC_CANARY="+canary)
	return inv
}

func stdinSeen(t *testing.T, inv Invocation) string {
	t.Helper()
	for _, kv := range inv.Env {
		if p, ok := strings.CutPrefix(kv, "MC_STDIN_OUT="); ok {
			b, _ := os.ReadFile(p)
			return string(b)
		}
	}
	return ""
}

func TestAdmissionRecords(t *testing.T) {
	want := map[string]string{"mosquitto": "BSD-3-Clause", "rabbitmqadmin": "MIT", "kcat": "BSD-2-Clause"}
	if got := strings.Join(Names(), ","); got != "kcat,mosquitto,rabbitmqadmin" {
		t.Fatalf("admitted clients = %s", got)
	}
	for n, lic := range want {
		c, _ := Lookup(n)
		if c.License != lic {
			t.Errorf("%s licence = %q, want pinned %q", n, c.License, lic)
		}
		if c.BrokerLicense == "" || c.BrokerLicense == c.License || c.Broker == "" || c.Fidelity == "" {
			t.Errorf("%s: broker/licence must be recorded separately from the client licence", n)
		}
		if len(c.Binaries) == 0 {
			t.Errorf("%s: no host binaries declared", n)
		}
	}
	if c, _ := Lookup("mosquitto"); len(c.Binaries) != 2 {
		t.Errorf("mosquitto must declare both pub and sub binaries: %v", c.Binaries)
	}
	if c, _ := Lookup("rabbitmqadmin"); !strings.Contains(c.BrokerLicense, "MPL-2.0") {
		t.Error("rabbitmqadmin broker licence must name MPL-2.0 for the RabbitMQ server")
	}
	if c, _ := Lookup("kcat"); !strings.Contains(c.BrokerLicense, "Apache-2.0") {
		t.Error("kcat broker licence must name Apache-2.0 for Apache Kafka")
	}
	if err := HostOnlyError("kcat"); !strings.Contains(err.Error(), "no sha256-pinned managed release") {
		t.Errorf("host-only diagnostic = %v", err)
	}
}

func TestResolveEndpoint(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	mqttBase := map[string]string{"MOSQUITTO_HOST": "mq.example.test", "MOSQUITTO_USER": "app", "MOSQUITTO_PASSWORD": canary}
	cases := []struct {
		name   string
		client string
		m      map[string]string
		code   gws.Code
	}{
		{"no host: ambient MQTT_HOST is not a fallback", "mosquitto", map[string]string{"MQTT_HOST": "x", "MOSQUITTO_PASSWORD": canary}, gws.CodeMissingCredentials},
		{"no credential for management api", "rabbitmqadmin", map[string]string{"RABBITMQ_HOST": "mq.example.test", "RABBITMQ_USER": "app"}, gws.CodeMissingCredentials},
		{"no user for management api", "rabbitmqadmin", map[string]string{"RABBITMQ_HOST": "mq.example.test", "RABBITMQ_PASSWORD": canary}, gws.CodeMissingCredentials},
		{"plaintext off-loopback", "mosquitto", merge(mqttBase, "MOSQUITTO_TLS", "plaintext-loopback"), gws.CodeInvalidInput},
		{"unknown tls", "kcat", map[string]string{"KAFKA_HOST": "k.example.test", "KAFKA_TLS": "none"}, gws.CodeInvalidInput},
		{"bad port", "kcat", map[string]string{"KAFKA_HOST": "k.example.test", "KAFKA_PORT": "99999"}, gws.CodeInvalidInput},
		{"both credential forms", "mosquitto", merge(mqttBase, "MOSQUITTO_PASSWORD_FILE", "/x"), gws.CodeInvalidInput},
		{"password without user", "kcat", map[string]string{"KAFKA_HOST": "k.example.test", "KAFKA_PASSWORD": canary}, gws.CodeInvalidInput},
		{"user without password cannot prompt", "kcat", map[string]string{"KAFKA_HOST": "k.example.test", "KAFKA_USER": "app"}, gws.CodeMissingCredentials},
	}
	for _, c := range cases {
		if _, err := ResolveEndpoint(c.client, env(c.m)); err == nil || err.Code != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	ep, err := ResolveEndpoint("mosquitto", env(mqttBase))
	if err != nil || ep.TLS != TLSVerify || ep.Port != 8883 {
		t.Fatalf("mosquitto default must be TLS verify on 8883: %+v %v", ep, err)
	}
	ep, err = ResolveEndpoint("kcat", env(map[string]string{"KAFKA_HOST": "k.example.test"}))
	if err != nil || ep.TLS != TLSVerify || ep.Port != 9093 || ep.Password != "" {
		t.Fatalf("kcat default must be TLS verify on 9093, anonymous allowed: %+v %v", ep, err)
	}
	ep, err = ResolveEndpoint("rabbitmqadmin", env(map[string]string{"RABBITMQ_HOST": "mq.example.test", "RABBITMQ_USER": "app", "RABBITMQ_PASSWORD": canary}))
	if err != nil || ep.Port != 15671 {
		t.Fatalf("rabbitmqadmin default must be TLS verify on 15671: %+v %v", ep, err)
	}
	if lb, err := ResolveEndpoint("kcat", env(map[string]string{"KAFKA_HOST": "127.0.0.1", "KAFKA_TLS": "plaintext-loopback"})); err != nil || lb.TLS != TLSPlaintextLoopback || lb.Port != 9092 {
		t.Errorf("loopback plaintext kcat should be allowed on 9092: %+v %v", lb, err)
	}
	pf := filepath.Join(t.TempDir(), "pw")
	_ = os.WriteFile(pf, []byte(canary+"\n"), 0o600)
	if f, err := ResolveEndpoint("mosquitto", env(map[string]string{"MOSQUITTO_HOST": "h", "MOSQUITTO_USER": "u", "MOSQUITTO_PASSWORD_FILE": pf})); err != nil || f.Password != canary {
		t.Errorf("password file: %v", err)
	}
	if _, err := ResolveEndpoint("mosquitto", env(map[string]string{"MOSQUITTO_HOST": "h", "MOSQUITTO_USER": "u", "MOSQUITTO_PASSWORD_FILE": "/nonexistent/" + canary})); err == nil || strings.Contains(err.Message, canary) {
		t.Errorf("unreadable password file must fail without echoing the path-borne canary: %v", err)
	}
	if s := ep.RedactedScope(); strings.Contains(s, canary) || !strings.Contains(s, "credential=<set>") {
		t.Errorf("scope leaks or omits: %s", s)
	}
	anon, _ := ResolveEndpoint("kcat", env(map[string]string{"KAFKA_HOST": "h"}))
	if !strings.Contains(anon.RedactedScope(), "credential=<anonymous>") {
		t.Errorf("anonymous scope: %s", anon.RedactedScope())
	}
}

func merge(m map[string]string, k, v string) map[string]string {
	out := map[string]string{k: v}
	for a, b := range m {
		if _, ok := out[a]; !ok {
			out[a] = b
		}
	}
	return out
}

func TestBuildSecretsNeverInArgv(t *testing.T) {
	payload := `{"n":1}`
	for _, n := range Names() {
		mode := ModePublish
		op := Op{Mode: mode, Topic: "t", Payload: payload}
		if n == "rabbitmqadmin" {
			op.Exchange = "amq.default"
		}
		inv, err := Build(endpoint(n), op, "/bin/"+n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if strings.Contains(strings.Join(inv.Args, " "), canary) {
			t.Errorf("%s: credential reached argv: %q", n, inv.Args)
		}
		// The payload travels on stdin (mosquitto, kcat) or a staged file
		// (rabbitmqadmin) — never argv.
		if !strings.Contains(strings.Join(inv.Args, " "), "@AUX/") && n == "rabbitmqadmin" {
			t.Errorf("%s: publish payload must reference a staged file, not argv", n)
		}
		if inv.Op.MaxMessages != DefaultMaxMessages || inv.Op.MaxBytes != DefaultMaxBytes || inv.Op.Timeout != DefaultTimeout {
			t.Errorf("%s: finite defaults not applied: %+v", n, inv.Op)
		}
	}
	// mosquitto carries the password as the first stdin line (the
	// interactive prompt handoff): assert the exact framing — password line
	// first, payload intact after — and nothing secret-shaped on argv.
	inv, err := Build(endpoint("mosquitto"), Op{Mode: ModePublish, Topic: "t", Payload: payload}, "mosquitto_pub")
	if err != nil {
		t.Fatal(err)
	}
	if inv.Stdin != canary+"\n"+payload {
		t.Errorf("mosquitto stdin framing wrong: %q", inv.Stdin)
	}
	if !strings.Contains(strings.Join(inv.Args, " "), "-s") {
		t.Errorf("mosquitto publish must send whole stdin as one message: %q", inv.Args)
	}
	// kcat SASL material stays in the staged config, never argv.
	inv, err = Build(endpoint("kcat"), Op{Mode: ModePublish, Topic: "t", Payload: payload}, "kcat")
	if err != nil {
		t.Fatal(err)
	}
	cfg := inv.Aux[auxKcatConfig]
	if !strings.Contains(cfg, "sasl.password="+canary) || !strings.Contains(cfg, "security.protocol=SASL_SSL") {
		t.Errorf("kcat config wrong:\n%s", cfg)
	}
	if strings.Contains(strings.Join(inv.Env, "\n"), canary) {
		t.Errorf("kcat must not duplicate the password into env")
	}
}

func TestTLSArgs(t *testing.T) {
	inv, err := Build(endpoint("mosquitto"), Op{Mode: ModeSubscribe, Topic: "t"}, "mosquitto_sub")
	if err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "--cafile /etc/ca.pem") {
		t.Errorf("mosquitto TLS args: %s", a)
	}
	if _, err := Build(Endpoint{Client: "mosquitto", Host: "h", Port: 8883, User: "u", TLS: TLSVerify, Password: canary}, Op{Mode: ModePublish, Topic: "t", Payload: "x"}, "x"); err == nil || err.Code != gws.CodeInvalidInput {
		t.Errorf("TLS verify without an explicit CA file must be refused loudly: %v", err)
	}
	inv, _ = Build(endpoint("kcat"), Op{Mode: ModeSubscribe, Topic: "t"}, "kcat")
	if c := inv.Aux[auxKcatConfig]; !strings.Contains(c, "security.protocol=SASL_SSL") || !strings.Contains(c, "ssl.ca.location=/etc/ca.pem") {
		t.Errorf("kcat TLS config: %q", c)
	}
	inv, _ = Build(endpoint("rabbitmqadmin"), Op{Mode: ModePublish, Topic: "q", Payload: "x"}, "rabbitmqadmin")
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "https://mq.example.test:15671") {
		t.Errorf("rabbitmqadmin server URL: %s", a)
	}
	if e := strings.Join(inv.Env, "\n"); !strings.Contains(e, "RABBITMQADMIN_PASSWORD="+canary) {
		t.Errorf("rabbitmqadmin password must travel by env: %s", e)
	}
}

func TestSubscribeAlwaysBounded(t *testing.T) {
	inv, _ := Build(endpoint("mosquitto"), Op{Mode: ModeSubscribe, Topic: "t"}, "mosquitto_sub")
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "-C 100") {
		t.Errorf("mosquitto_sub must disconnect after N messages (-C): %s", a)
	}
	inv, _ = Build(endpoint("kcat"), Op{Mode: ModeSubscribe, Topic: "t"}, "kcat")
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "-c 100") || !strings.Contains(a, "-J") {
		t.Errorf("kcat consume must be count-capped JSON: %s", a)
	}
	inv, _ = Build(endpoint("rabbitmqadmin"), Op{Mode: ModeSubscribe, Topic: "q"}, "rabbitmqadmin")
	if a := strings.Join(inv.Args, " "); !strings.Contains(a, "--count 100") || !strings.Contains(a, "ack_requeue_true") {
		t.Errorf("rabbitmqadmin get must be count-capped and requeueing: %s", a)
	}
	// There is no spelling for an unbounded listener: clearing the bounds
	// re-applies finite defaults instead of disabling them.
	inv, _ = Build(endpoint("kcat"), Op{Mode: ModeSubscribe, Topic: "t"}, "kcat")
	if inv.Op.MaxMessages != DefaultMaxMessages || inv.Op.Timeout != DefaultTimeout {
		t.Errorf("bounds must default finite: %+v", inv.Op)
	}
}

func TestPublishEffectsExplicit(t *testing.T) {
	res, err := Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModePublish, Topic: "t", Payload: `{"a":1}`}, "kcat_consume.out", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.Mutation != "published" {
		t.Errorf("publish must report its effect: %+v", res)
	}
	res, err = Run(context.Background(), helperInv(t, "mosquitto", Op{Mode: ModeSubscribe, Topic: "t"}, "mosquitto_sub.out", ""))
	if err != nil || res.Mutation != "none" {
		t.Fatalf("subscribe must report no mutation: %v %+v", err, res)
	}
	// A cancelled publish is an unknown outcome, never a silent success.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err = Run(ctx, helperInv(t, "rabbitmqadmin", Op{Mode: ModePublish, Topic: "q", Payload: "x"}, "rabbitmq_get.out", "sleep"))
	if err == nil || err.Code != gws.CodePartialMutation || !strings.Contains(err.Message, "UNKNOWN") {
		t.Fatalf("interrupted publish must be partial-unknown: %v", err)
	}
}

func TestBuildRefusals(t *testing.T) {
	big := strings.Repeat("x", MaxPayload+1)
	cases := []struct {
		name   string
		client string
		op     Op
		code   gws.Code
	}{
		{"mode required", "kcat", Op{Topic: "t", Payload: "x"}, gws.CodeInvalidInput},
		{"empty topic", "kcat", Op{Mode: ModePublish, Topic: "  ", Payload: "x"}, gws.CodeInvalidInput},
		{"subscribe with payload", "kcat", Op{Mode: ModeSubscribe, Topic: "t", Payload: "x"}, gws.CodeInvalidInput},
		{"subscribe with exchange", "rabbitmqadmin", Op{Mode: ModeSubscribe, Topic: "q", Exchange: "e"}, gws.CodeInvalidInput},
		{"qos outside mqtt", "kcat", Op{Mode: ModeSubscribe, Topic: "t", QoS: 1, QoSSet: true}, gws.CodeUnsupported},
		{"qos out of range", "mosquitto", Op{Mode: ModePublish, Topic: "t", Payload: "x", QoS: 3, QoSSet: true}, gws.CodeInvalidInput},
		{"exchange outside rabbitmq", "mosquitto", Op{Mode: ModePublish, Topic: "t", Payload: "x", Exchange: "e"}, gws.CodeUnsupported},
		{"oversize payload", "kcat", Op{Mode: ModePublish, Topic: "t", Payload: big}, gws.CodeOutputLimit},
		{"messages over cap", "mosquitto", Op{Mode: ModeSubscribe, Topic: "t", MaxMessages: CapMaxMessages + 1}, gws.CodeInvalidInput},
		{"timeout over cap", "kcat", Op{Mode: ModeSubscribe, Topic: "t", Timeout: CapTimeout + 1}, gws.CodeInvalidInput},
		{"unknown client", "nats", Op{Mode: ModePublish, Topic: "t", Payload: "x"}, gws.CodeUnsupported},
	}
	for _, c := range cases {
		if _, err := Build(endpoint(c.client), c.op, "x"); err == nil || err.Code != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
}

func TestRunRecordedFixtures(t *testing.T) {
	res, err := Run(context.Background(), helperInv(t, "mosquitto", Op{Mode: ModeSubscribe, Topic: "sensors/#"}, "mosquitto_sub.out", ""))
	if err != nil {
		t.Fatal(err)
	}
	if res.RecordCount != 2 || res.Partial || res.Mutation != "none" {
		t.Fatalf("mosquitto result: %+v", res)
	}
	m0 := res.Records[0].(map[string]any)
	if m0["topic"] != "sensors/temp" || m0["payload"] != `{"t":21.5,"n":9007199254740993}` {
		t.Errorf("framed record wrong: %#v", m0)
	}
	m1 := res.Records[1].(map[string]any)
	if m1["payload"] != "line1\nline2" {
		t.Errorf("multiline payload mangled: %#v", m1)
	}

	res, err = Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModeSubscribe, Topic: "orders"}, "kcat_consume.out", ""))
	if err != nil || res.RecordCount != 2 || res.Partial {
		t.Fatalf("kcat: %v %+v", err, res)
	}
	b, _ := json.Marshal(res.Records)
	if !strings.Contains(string(b), "9007199254740993") {
		t.Errorf("kcat 64-bit integer lost precision: %s", b)
	}
	if !strings.Contains(string(b), `"key":null`) || !strings.Contains(string(b), `"offset":7`) {
		t.Errorf("kcat metadata lost: %s", b)
	}

	res, err = Run(context.Background(), helperInv(t, "rabbitmqadmin", Op{Mode: ModeSubscribe, Topic: "jobs"}, "rabbitmq_get.out", ""))
	if err != nil || res.RecordCount != 1 || res.Partial {
		t.Fatalf("rabbitmqadmin: %v %+v", err, res)
	}
	b, _ = json.Marshal(res.Records)
	if !strings.Contains(string(b), `12345678901234567890`) {
		t.Errorf("rabbitmq decimal precision lost: %s", b)
	}
}

func TestEnvScrubbedOfAmbientVars(t *testing.T) {
	out := childEnv("mosquitto", []string{"MQTT_HOST=evil", "MOSQUITTO_PASSWORD=ambient", "PATH=/bin"}, []string{"PATH=/bin"})
	j := strings.Join(out, "\n")
	if strings.Contains(j, "evil") || strings.Contains(j, "ambient") || !strings.Contains(j, "PATH=/bin") {
		t.Errorf("ambient scrub wrong:\n%s", j)
	}
}

func TestBoundedReads(t *testing.T) {
	res, err := Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModeSubscribe, Topic: "t", MaxMessages: 1}, "kcat_consume.out", ""))
	if err != nil || !res.Partial || res.Reason != "row-limit" || res.RecordCount != 1 {
		t.Fatalf("message limit must be explicit partial: %v %+v", err, res)
	}
	res, err = Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModeSubscribe, Topic: "t", MaxBytes: 120}, "kcat_consume.out", ""))
	if err != nil || !res.Partial || res.Reason != "byte-limit" || res.RecordCount != 1 {
		t.Fatalf("byte limit must drop the cut-off tail and say partial: %v %+v", err, res)
	}
	start := time.Now()
	inv := helperInv(t, "mosquitto", Op{Mode: ModeSubscribe, Topic: "t", MaxBytes: 1 << 20}, "mosquitto_sub.out", "flood")
	res, err = Run(context.Background(), inv)
	if err != nil || !res.Partial || res.Reason != "byte-limit" || res.BytesRead != 1<<20 {
		t.Fatalf("unbounded producer must be stopped at the bound: %v %+v", err, res)
	}
	if time.Since(start) > 20*time.Second {
		t.Error("flooding child was not killed promptly")
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := Run(ctx, helperInv(t, "mosquitto", Op{Mode: ModeSubscribe, Topic: "t"}, "mosquitto_sub.out", "sleep"))
	if err == nil || err.Code != gws.CodeCancelled || err.Exit() != 130 || !strings.Contains(err.Message, "nothing was published") {
		t.Fatalf("subscribe cancel: %v", err)
	}
	_, err = Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModePublish, Topic: "t", Payload: "x", Timeout: time.Second}, "kcat_consume.out", "sleep"))
	if err == nil || err.Code != gws.CodePartialMutation || !strings.Contains(err.Message, "UNKNOWN") {
		t.Fatalf("publish timeout must be partial-unknown: %v", err)
	}
	_, err = Run(context.Background(), helperInv(t, "kcat", Op{Mode: ModeSubscribe, Topic: "t", Timeout: time.Second}, "kcat_consume.out", "sleep"))
	if err == nil || err.Code != gws.CodeTimeout || err.Exit() != 124 || !err.Retryable {
		t.Fatalf("subscribe timeout: %v", err)
	}
}

func TestRedactionCanary(t *testing.T) {
	inv := helperInv(t, "rabbitmqadmin", Op{Mode: ModeSubscribe, Topic: "q"}, "auth_fail.err", "fail")
	_, err := Run(context.Background(), inv)
	if err == nil || err.Code != gws.CodeMissingCredentials {
		t.Fatalf("auth failure must classify: %v", err)
	}
	var buf bytes.Buffer
	err.EmitError(&buf, "msgclients op")
	for name, s := range map[string]string{"message": err.Message, "envelope": buf.String(), "explain": fmt.Sprint(inv.Explain(endpoint("rabbitmqadmin"))), "scope": endpoint("rabbitmqadmin").RedactedScope()} {
		if strings.Contains(s, canary) {
			t.Errorf("canary leaked into %s: %s", name, s)
		}
	}
	if !strings.Contains(err.Message, gws.Redacted) {
		t.Errorf("provider error body should carry the redaction marker: %s", err.Message)
	}
	// payload-borne secrets are masked in the preview too
	pre, _ := Build(endpoint("kcat"), Op{Mode: ModePublish, Topic: "t", Payload: `{"pw":"` + canary + `"}`}, "kcat")
	if strings.Contains(fmt.Sprint(pre.Explain(endpoint("kcat"))), canary) {
		t.Error("explain leaked a declared secret from the payload")
	}
}

func TestAuxFilesStagedAndRemoved(t *testing.T) {
	inv, err := Build(endpoint("kcat"), Op{Mode: ModePublish, Topic: "t", Payload: "x"}, "kcat")
	if err != nil {
		t.Fatal(err)
	}
	subst, cleanup, merr := materializeAux(inv.Aux)
	if merr != nil {
		t.Fatal(merr)
	}
	path := subst(auxKcatConfig)
	if path == auxKcatConfig {
		t.Fatal("sentinel was not substituted")
	}
	fi, serr := os.Stat(path)
	if serr != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged file must be 0600: %v %v", path, serr)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "sasl.password="+canary) {
		t.Errorf("staged content wrong:\n%s", b)
	}
	cleanup()
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("staged file not removed: %s", path)
	}
}

func TestExecuteParityAndExplain(t *testing.T) {
	env := map[string]string{"KAFKA_HOST": "k.example.test", "KAFKA_USER": "app", "KAFKA_PASSWORD": canary}
	getenv := func(k string) string { return env[k] }
	if err := Schema().Validate(); err != nil {
		t.Fatalf("schema invalid: %v", err)
	}
	cli := Values{"client": "kcat", "mode": "publish", "topic": "t", "payload": `{"a":1}`, "explain": "true", "max-messages": "5"}
	mcp := Values{"payload": `{"a":1}`, "explain": "true", "topic": "t", "mode": "publish", "max-messages": "5", "client": "kcat"}
	var a, b bytes.Buffer
	ea := Execute(context.Background(), &a, getenv, nil, cli)
	eb := Execute(context.Background(), &b, getenv, nil, mcp)
	if ea != 0 || ea != eb || a.String() != b.String() {
		t.Fatalf("CLI and MCP must yield identical results:\n%s\n%s", a.String(), b.String())
	}
	if !strings.Contains(a.String(), `"schema_version"`) || strings.Contains(a.String(), canary) || !strings.Contains(a.String(), "PUBLISHED") {
		t.Errorf("explain envelope: %s", a.String())
	}
	for _, bad := range []Values{
		{"client": "kcat", "mode": "publish", "topic": "t", "payload": "x", "bogus": "1"},
		{"client": "kcat", "mode": "publish", "topic": "t", "payload": "x", "max-messages": "-1"},
		{"client": "nats", "mode": "publish", "topic": "t", "payload": "x"},
		{"client": "kcat", "topic": "t", "payload": "x"},
	} {
		var x, y bytes.Buffer
		cx := Execute(context.Background(), &x, getenv, nil, bad)
		cy := Execute(context.Background(), &y, getenv, nil, bad)
		if cx == 0 || cx != cy || x.String() != y.String() || !strings.Contains(x.String(), `"schema_version"`) {
			t.Errorf("rejection must be typed and identical: %v %d", bad, cx)
		}
	}
	var m bytes.Buffer
	missing := func(string) (string, error) { return "", os.ErrNotExist }
	if c := Execute(context.Background(), &m, getenv, missing, Values{"client": "kcat", "mode": "subscribe", "topic": "t"}); c != 2 || !strings.Contains(m.String(), "no sha256-pinned managed release") {
		t.Errorf("absent host client must be an actionable refusal (exit 2): %d %s", c, m.String())
	}
	var nores bytes.Buffer
	if c := Execute(context.Background(), &nores, func(string) string { return "" }, nil, Values{"client": "kcat", "mode": "subscribe", "topic": "t"}); c == 0 {
		t.Error("no explicit endpoint must fail")
	}
}

func TestEmitEnvelope(t *testing.T) {
	res, err := Parse("kcat", "t", strings.NewReader(`{"a":1}`), Op{Mode: ModeSubscribe})
	if err != nil {
		t.Fatal(err)
	}
	res.Mutation, res.Scope = "published", "x"
	var buf bytes.Buffer
	if Emit(&buf, "msgclients op", res) != 0 || !strings.Contains(buf.String(), `"schema_version"`) {
		t.Errorf("envelope: %s", buf.String())
	}
}

func TestParseRejectsGarbageJSON(t *testing.T) {
	if _, err := Parse("kcat", "t", strings.NewReader("not json"), Op{Mode: ModeSubscribe}); err == nil || err.Code != gws.CodeProviderFailure {
		t.Errorf("non-JSON output must be refused, not guessed: %v", err)
	}
}

func TestPublishBinSelection(t *testing.T) {
	if publishBin("mosquitto", ModePublish) != "mosquitto_pub" || publishBin("mosquitto", ModeSubscribe) != "mosquitto_sub" {
		t.Error("mosquitto must dispatch to its pub/sub binaries by mode")
	}
	if publishBin("kcat", ModePublish) != "kcat" || publishBin("rabbitmqadmin", ModeSubscribe) != "rabbitmqadmin" {
		t.Error("single-binary clients must resolve their own binary")
	}
}
