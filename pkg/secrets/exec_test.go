package secrets

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Run the test executable itself, so no host shell or provider is needed.
// Check secrets in memory and print only a non-secret receipt.
func TestSecretExecChild(t *testing.T) {
	mode := os.Getenv("BASHY_SECRET_EXEC_TEST_CHILD")
	if mode == "" {
		return
	}
	if mode == "fail" {
		os.Exit(23)
	}
	if mode == "stdin-hash" {
		// The stdin handle route's fake consumer: hash ALL of stdin and
		// print only the digest, so the test can prove the child received
		// exactly the secret bytes without the value ever being echoed.
		h := sha256.New()
		if _, err := io.Copy(h, os.Stdin); err != nil {
			os.Exit(27)
		}
		fmt.Printf("%x\n", h.Sum(nil))
		os.Exit(0)
	}
	if os.Getenv("DO_ORBIT_AGENT") != "synthetic-agent" ||
		os.Getenv("DOCR_DHNT_PUSH_TOKEN") != "synthetic-push" ||
		os.Getenv("CUSTOM_TOKEN") != "synthetic-custom" ||
		os.Getenv("UNRELATED_INHERITED") != "keep" {
		os.Exit(24)
	}
	if _, exists := os.LookupEnv("VAULT_ONLY"); exists {
		os.Exit(25)
	}
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "--child-flag" {
		os.Exit(26)
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	fmt.Fprintln(os.Stderr, "child completed")
	os.Exit(0)
}

func TestSecretExecChildEnvironmentOnly(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["do/orbit/agent"] = "synthetic-agent"
	fv.data["docr/dhnt/push-token"] = "synthetic-push"
	fv.data["custom"] = "synthetic-custom"
	fv.data["VAULT_ONLY"] = "synthetic-unrequested"
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "check")
	t.Setenv("DO_ORBIT_AGENT", "parent-value")
	t.Setenv("UNRELATED_INHERITED", "keep")
	for _, name := range []string{"DOCR_DHNT_PUSH_TOKEN", "CUSTOM_TOKEN", "VAULT_ONLY"} {
		t.Setenv(name, "") // arrange restoration at cleanup before unsetting
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	before := strings.Join(os.Environ(), "\n")
	out, stderr, err := executeSecrets(t, fv.cfg(), strings.NewReader("child input\n"),
		"exec", "do/orbit/agent", "docr/dhnt/push-token", "custom=CUSTOM_TOKEN", "--",
		os.Args[0], "-test.run=^TestSecretExecChild$", "--", "--child-flag")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out != "child input\n" || stderr != "child completed\n" {
		t.Fatal("exec did not preserve child stdio or printed unexpected data")
	}
	if strings.Join(os.Environ(), "\n") != before {
		t.Fatal("exec changed the parent environment")
	}
	for _, dir := range []string{os.Getenv("BASHY_HOME"), os.Getenv("XDG_CACHE_HOME"), os.Getenv("XDG_CONFIG_HOME")} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("exec wrote local state")
		}
	}
	if len(fv.paths) != 3 {
		t.Fatalf("wanted only three individual fetches, got %d requests", len(fv.paths))
	}
}

func TestSecretExecUnknownName(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["known"] = "synthetic-known"
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "fail")
	out, stderr, err := executeSecrets(t, fv.cfg(), nil, "exec", "known", "do/missing/agent", "--", os.Args[0], "-test.run=^TestSecretExecChild$")
	if err == nil || err.Error() != "no such secret: do/missing/agent" {
		t.Fatalf("unknown name: %v", err)
	}
	if out != "" || stderr != "" {
		t.Fatal("unknown name produced output or launched the child")
	}
}

func TestSecretExecChildFailure(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["known"] = "synthetic-known"
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "fail")
	_, _, err := executeSecrets(t, fv.cfg(), nil, "exec", "known", "--", os.Args[0], "-test.run=^TestSecretExecChild$")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("child failure not returned: %v", err)
	}
}

// TestSecretExecStdinHandle is the --stdin red/green pair: the fake child
// (mode stdin-hash) echoes only a hash of its stdin, and the test proves the
// child received EXACTLY the secret bytes. Red (no --stdin flag): the verb
// refuses the invocation as an unknown flag, so a stdin consumer has no
// handle route once show/env refuse non-TTY callers. Green: the digest over
// the child's stdin equals the digest of the value, one vault fetch, and the
// value itself never appears in any captured byte stream or argv.
func TestSecretExecStdinHandle(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	const value = "s3cret-stdin-pw-do-not-print"
	fv.data["host-a/outpost/pw"] = value
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "stdin-hash")

	out, stderr, err := executeSecrets(t, fv.cfg(), strings.NewReader("parent console input\n"),
		"exec", "--stdin", "host-a/outpost/pw", "--",
		os.Args[0], "-test.run=^TestSecretExecChild$")
	if err != nil {
		t.Fatalf("exec --stdin: %v", err)
	}
	want := fmt.Sprintf("%x\n", sha256.Sum256([]byte(value)))
	if out != want {
		t.Fatalf("child stdin digest = %q, want hash of the secret value (%q)", out, want)
	}
	if strings.Contains(out+stderr, value) {
		t.Fatal("secret value appeared in captured output")
	}
	if len(fv.paths) != 1 {
		t.Fatalf("wanted exactly one vault fetch, got %d", len(fv.paths))
	}
}

// The child gets the value on stdin INSTEAD of the parent's stdin: the hash
// in TestSecretExecStdinHandle is over exactly the value bytes (no parent
// console input mixed in, EOF right after the value), and this companion
// pins that the parent's own stdin is never forwarded in --stdin mode.
func TestSecretExecStdinReplacesParentStdin(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	const value = "just-the-value-nothing-else"
	fv.data["one/name"] = value
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "stdin-hash")

	out, _, err := executeSecrets(t, fv.cfg(), strings.NewReader("PARENT CONSOLE BYTES THAT MUST NOT REACH THE CHILD"),
		"exec", "--stdin", "one/name", "--",
		os.Args[0], "-test.run=^TestSecretExecChild$")
	if err != nil {
		t.Fatalf("exec --stdin: %v", err)
	}
	if want := fmt.Sprintf("%x\n", sha256.Sum256([]byte(value))); out != want {
		t.Fatalf("child stdin = parent input + value or wrong bytes: %q", out)
	}
}

func TestSecretExecStdinUnknownName(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["known"] = "synthetic-known"
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "fail")
	out, stderr, err := executeSecrets(t, fv.cfg(), nil, "exec", "--stdin", "do/missing/pw", "--", os.Args[0], "-test.run=^TestSecretExecChild$")
	if err == nil || err.Error() != "no such secret: do/missing/pw" {
		t.Fatalf("unknown --stdin name: %v", err)
	}
	if out != "" || stderr != "" {
		t.Fatal("unknown --stdin name produced output or launched the child")
	}
}

func TestSecretExecStdinInvalidArguments(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["known"] = "synthetic-known"
	t.Setenv("BASHY_SECRET_EXEC_TEST_CHILD", "fail")
	for _, args := range [][]string{
		{"exec", "--stdin"}, // flag needs its NAME value
		{"exec", "--stdin", "known", "extra", "--", "command"}, // positional names alongside --stdin
		{"exec", "--stdin", "known=VAR", "--", "command"},      // binding form refused
		{"exec", "--stdin", "", "--", "command"},               // empty name
		{"exec", "--stdin", "known", "--"},                     // no command
		{"exec", "--stdin", "known", "command"},                // missing --
	} {
		if _, _, err := executeSecrets(t, fv.cfg(), nil, args...); err == nil {
			t.Errorf("accepted invalid arguments %q", args)
		}
	}
	if len(fv.paths) != 0 {
		t.Fatal("invalid --stdin arguments fetched secrets")
	}
}

func TestSecretExecInvalidArguments(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	for _, args := range [][]string{
		{"exec"}, {"exec", "name"}, {"exec", "name", "--"},
		{"exec", "--", "command"}, {"exec", "name", "command"},
		{"exec", "name=", "--", "command"}, {"exec", "=TOKEN", "--", "command"},
		{"exec", "name=1TOKEN", "--", "command"}, {"exec", "name.with.dot", "--", "command"},
	} {
		if _, _, err := executeSecrets(t, fv.cfg(), nil, args...); err == nil {
			t.Errorf("accepted invalid arguments %q", args)
		}
	}
	if len(fv.paths) != 0 {
		t.Fatal("invalid arguments fetched secrets")
	}
}

func TestEnvRefusesBareVaultNamesBeforeOutputOrCache(t *testing.T) {
	allowRevealForTest(t) // simulate the interactive owner terminal
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	fv.data["NAME"] = "synthetic-value"
	fv.data["do/orbit/agent"] = "synthetic-agent"
	for _, rhs := range []string{"NAME", "do/orbit/agent", "NAME # mistaken reference"} {
		for _, noCache := range []bool{false, true} {
			writeDefaultTemplate(t, "EDITOR=vim\nTOKEN="+rhs+"\n")
			const cached = "# old cached render\n"
			if err := writeCache(cacheFile(), []byte(cached)); err != nil {
				t.Fatal(err)
			}
			args := []string{"env"}
			if noCache {
				args = append(args, "--no-cache")
			}
			out, _, err := executeSecrets(t, fv.cfg(), nil, args...)
			if err == nil || !strings.Contains(err.Error(), "did you mean @"+strings.Fields(rhs)[0]+"?") {
				t.Fatalf("bare name was not refused: %v", err)
			}
			if out != "" {
				t.Fatal("ambiguous template emitted exports or cached output")
			}
			data, err := os.ReadFile(cacheFile())
			if err != nil || string(data) != cached {
				t.Fatal("refused template changed the cache")
			}
		}
	}
	writeDefaultTemplate(t, "EDITOR=vim\nTOKEN=@NAME\nQUOTED='NAME'\nDOUBLE=\"NAME\"\n")
	out, _, err := executeSecrets(t, fv.cfg(), nil, "env", "--no-cache")
	if err != nil || digest(out) != digest("export DOUBLE='NAME'\nexport EDITOR='vim'\nexport QUOTED='NAME'\nexport TOKEN='synthetic-value'\n") {
		t.Fatal("references or intentional literals stopped working")
	}
}

func TestSecretListPrefix(t *testing.T) {
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	for _, name := range []string{"do/orbit/agent", "do/orbit2/agent", "docr/dhnt/push"} {
		fv.data[name] = "synthetic-value"
	}
	for _, tc := range []struct{ prefix, want string }{
		{"do/orbit/", "do/orbit/agent\n"},
		{"do/orbit", "do/orbit/agent\ndo/orbit2/agent\n"},
		{"do", "do/orbit/agent\ndo/orbit2/agent\ndocr/dhnt/push\n"},
		{"missing", ""}, {"do/*", ""},
	} {
		for _, verb := range []string{"ls", "list"} {
			out, _, err := executeSecrets(t, fv.cfg(), nil, verb, tc.prefix)
			if err != nil || out != tc.want {
				t.Fatalf("%s prefix %q: output %q, error %v", verb, tc.prefix, out, err)
			}
		}
	}
}

func TestSecretNamesEscapedRoundTrip(t *testing.T) {
	allowRevealForTest(t) // simulate the interactive owner terminal
	configureSecretsTestEnv(t)
	fv := newFakeVault(t)
	for _, tc := range []struct{ name, escaped string }{
		{"do/orbit/agent", "do%2Forbit%2Fagent"},
		{"docr/dhnt/push", "docr%2Fdhnt%2Fpush"},
		{"provider/a%2Fb/key?#", "provider%2Fa%252Fb%2Fkey%3F%23"},
	} {
		if _, _, err := executeSecrets(t, fv.cfg(), nil, "set", tc.name, "synthetic-value"); err != nil {
			t.Fatal(err)
		}
		out, _, err := executeSecrets(t, fv.cfg(), nil, "show", tc.name)
		if err != nil || digest(out) != digest("synthetic-value\n") {
			t.Fatal("named secret did not round-trip")
		}
		if got := fv.paths[len(fv.paths)-1]; got != "GET /api/v1/secrets/"+tc.escaped {
			t.Fatalf("GET path = %q", got)
		}
		if _, _, err := executeSecrets(t, fv.cfg(), nil, "rm", tc.name); err != nil {
			t.Fatal(err)
		}
		if got := fv.paths[len(fv.paths)-1]; got != "DELETE /api/v1/secrets/"+tc.escaped {
			t.Fatalf("DELETE path = %q", got)
		}
		if _, exists := fv.data[tc.name]; exists {
			t.Fatal("delete retained the secret")
		}
	}
}

func TestSecretHelpConvention(t *testing.T) {
	configureSecretsTestEnv(t)
	out, _, err := executeSecrets(t, Config{}, nil, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<provider>/<account>/<purpose>", "do/orbit/agent", "docr/dhnt/push", "~/.ssh/", "secret exec"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q", want)
		}
	}
}
