package secrets

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runGenCmd runs `secret gen ARGS` with NO --url/--token and every ambient
// token/url source blanked: gen must work with nothing to resolve.
func runGenCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "")
	cmd := newSecretsCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"gen"}, args...))
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func hasClass(pw, class string) bool { return strings.ContainsAny(pw, class) }

func TestGenDefault(t *testing.T) {
	out, errb, err := runGenCmd(t)
	if err != nil || errb != "" {
		t.Fatalf("gen: err=%v stderr=%q", err, errb)
	}
	got := lines(out)
	if len(got) != 1 {
		t.Fatalf("want one line, got %q", out)
	}
	pw := got[0]
	if len([]rune(pw)) != genDefaultLength {
		t.Fatalf("length %d, want %d: %q", len([]rune(pw)), genDefaultLength, pw)
	}
	alphabet := genLower + genUpper + genDigits + genSymbols
	for _, ch := range pw {
		if !strings.ContainsRune(alphabet, ch) {
			t.Fatalf("character %q outside the default alphabet in %q", ch, pw)
		}
	}
	for _, class := range []string{genLower, genUpper, genDigits, genSymbols} {
		if !hasClass(pw, class) {
			t.Fatalf("password %q is missing class %q", pw, class)
		}
	}
}

func TestGenPositionalLength(t *testing.T) {
	out, _, err := runGenCmd(t, "32")
	if err != nil || len([]rune(lines(out)[0])) != 32 {
		t.Fatalf("gen 32: err=%v out=%q", err, out)
	}
	if _, _, err := runGenCmd(t, "32", "--length", "16"); err == nil {
		t.Fatal("LENGTH and --length together must be refused")
	}
	if _, _, err := runGenCmd(t, "abc"); err == nil {
		t.Fatal("non-numeric LENGTH must be refused")
	}
}

// Length 4 with four required classes is the hardest accepted recipe
// (acceptance rate ≈ 7%): every draw must still satisfy the policy.
func TestGenRejectionLoopHoldsAtMinimumLength(t *testing.T) {
	out, _, err := runGenCmd(t, "-l", "4", "-n", "200")
	if err != nil {
		t.Fatalf("gen -l 4: %v", err)
	}
	got := lines(out)
	if len(got) != 200 {
		t.Fatalf("want 200 lines, got %d", len(got))
	}
	for _, pw := range got {
		for _, class := range []string{genLower, genUpper, genDigits, genSymbols} {
			if !hasClass(pw, class) {
				t.Fatalf("password %q is missing class %q", pw, class)
			}
		}
	}
	_, _, err = runGenCmd(t, "-l", "3")
	if err == nil || !strings.Contains(err.Error(), "cannot contain") {
		t.Fatalf("gen -l 3 with four classes must be impossible, got %v", err)
	}
}

func TestGenClassFlags(t *testing.T) {
	cases := []struct {
		flag    string
		dropped string
	}{
		{"--no-lower", genLower},
		{"--no-upper", genUpper},
		{"--no-digits", genDigits},
		{"--no-symbols", genSymbols},
	}
	for _, c := range cases {
		out, _, err := runGenCmd(t, c.flag, "-n", "20")
		if err != nil {
			t.Fatalf("%s: %v", c.flag, err)
		}
		for _, pw := range lines(out) {
			if hasClass(pw, c.dropped) {
				t.Fatalf("%s: %q still contains a dropped character", c.flag, pw)
			}
		}
	}
	_, _, err := runGenCmd(t, "--no-lower", "--no-upper", "--no-digits", "--no-symbols")
	if err == nil || !strings.Contains(err.Error(), "every character class is disabled") {
		t.Fatalf("all classes disabled must be refused, got %v", err)
	}
}

func TestGenCustomSymbols(t *testing.T) {
	out, _, err := runGenCmd(t, "--symbols", "@@", "--no-lower", "--no-upper", "--no-digits", "-l", "5")
	if err == nil {
		t.Fatalf("a one-character alphabet must be refused, got %q", out)
	}
	out, _, err = runGenCmd(t, "--symbols", "@#", "--no-lower", "--no-upper", "--no-digits", "-l", "8", "-n", "20")
	if err != nil {
		t.Fatalf("custom symbols: %v", err)
	}
	// @# is ONE class: the guarantee is one symbol per password, not one of
	// each symbol ("########" is valid, p=2/256 each). Both symbols must
	// still be drawn across the batch (160 draws; p(miss)=2^-159).
	for _, pw := range lines(out) {
		if strings.Trim(pw, "@#") != "" {
			t.Fatalf("%q is not drawn from @#", pw)
		}
	}
	if !strings.Contains(out, "@") || !strings.Contains(out, "#") {
		t.Fatalf("custom symbols @# not both drawn across the batch: %q", out)
	}
}

func TestGenNoAmbiguous(t *testing.T) {
	out, _, err := runGenCmd(t, "-B", "-n", "500")
	if err != nil {
		t.Fatalf("gen -B: %v", err)
	}
	for _, pw := range lines(out) {
		if strings.ContainsAny(pw, genAmbiguous) {
			t.Fatalf("%q contains an ambiguous glyph", pw)
		}
	}
	// A class that -B empties is an error, not a silent drop.
	_, _, err = runGenCmd(t, "-B", "--symbols", "B8")
	if err == nil || !strings.Contains(err.Error(), "symbols class is empty") {
		t.Fatalf("emptied class must be refused, got %v", err)
	}
}

func TestGenCharset(t *testing.T) {
	out, _, err := runGenCmd(t, "--charset", "0123456789abcdef", "-l", "64", "-n", "10")
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	for _, pw := range lines(out) {
		if len(pw) != 64 || strings.Trim(pw, "0123456789abcdef") != "" {
			t.Fatalf("%q is not a 64-char hex string", pw)
		}
	}
	// Duplicates in the charset must not weight a character twice.
	var env struct {
		AlphabetSize int     `json:"alphabet_size"`
		EntropyBits  float64 `json:"entropy_bits"`
	}
	out, _, err = runGenCmd(t, "--charset", "aab", "-l", "10", "--json")
	if err != nil || json.Unmarshal([]byte(out), &env) != nil {
		t.Fatalf("charset aab --json: err=%v out=%q", err, out)
	}
	if env.AlphabetSize != 2 || env.EntropyBits != 10 {
		t.Fatalf("charset aab: alphabet_size=%d entropy=%v, want 2 and 10", env.AlphabetSize, env.EntropyBits)
	}
	if _, _, err := runGenCmd(t, "--charset", "x"); err == nil {
		t.Fatal("single-character charset must be refused")
	}
}

func TestGenCountDistinct(t *testing.T) {
	out, _, err := runGenCmd(t, "-n", "50")
	if err != nil {
		t.Fatalf("gen -n 50: %v", err)
	}
	got := lines(out)
	seen := map[string]bool{}
	for _, pw := range got {
		if seen[pw] {
			t.Fatalf("duplicate password %q in 50 draws of ~130 bits", pw)
		}
		seen[pw] = true
	}
	if len(seen) != 50 {
		t.Fatalf("want 50 distinct passwords, got %d", len(seen))
	}
	for _, bad := range []string{"0", "-1", "10001"} {
		if _, _, err := runGenCmd(t, "-n", bad); err == nil {
			t.Fatalf("count %s must be refused", bad)
		}
	}
	for _, bad := range []string{"0", "1025"} {
		if _, _, err := runGenCmd(t, "-l", bad); err == nil {
			t.Fatalf("length %s must be refused", bad)
		}
	}
}

func TestGenJSONEnvelope(t *testing.T) {
	out, errb, err := runGenCmd(t, "--json", "-n", "3", "-l", "12")
	if err != nil || errb != "" {
		t.Fatalf("gen --json: err=%v stderr=%q", err, errb)
	}
	var env struct {
		Schema       string   `json:"schema_version"`
		Length       int      `json:"length"`
		Count        int      `json:"count"`
		AlphabetSize int      `json:"alphabet_size"`
		EntropyBits  float64  `json:"entropy_bits"`
		Passwords    []string `json:"passwords"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	if env.Schema != genSchemaVersion || env.Length != 12 || env.Count != 3 || len(env.Passwords) != 3 {
		t.Fatalf("envelope = %+v", env)
	}
	if env.AlphabetSize != 26+26+10+len(genSymbols) {
		t.Fatalf("alphabet_size = %d", env.AlphabetSize)
	}
	// With the per-class guarantee the entropy is strictly below the
	// unconstrained length·log2(alphabet), and not by much at length 12.
	naive := 12 * math.Log2(float64(env.AlphabetSize))
	if env.EntropyBits >= naive || env.EntropyBits < naive-1 {
		t.Fatalf("entropy_bits = %v, want just under %v", env.EntropyBits, naive)
	}
}

// Closed forms for the entropy accounting.
func TestGenEntropyClosedForms(t *testing.T) {
	// No required classes: length·log2(alphabet) exactly.
	r := &genRecipe{length: 8, alphabet: []rune("0123456789abcdef")}
	if got := r.entropyBits(); got != 32 {
		t.Fatalf("hex/8 entropy = %v, want 32", got)
	}
	if got := r.acceptRate(); got != 1 {
		t.Fatalf("unconstrained accept rate = %v, want 1", got)
	}
	// Two disjoint classes {a,b} and {c}, length 2: acceptable strings are
	// ac, bc, ca, cb = 4 → 2 bits; accept rate 4/9.
	r = &genRecipe{length: 2, alphabet: []rune("abc"), required: [][]rune{[]rune("ab"), []rune("c")}}
	if got := r.acceptable().Int64(); got != 4 {
		t.Fatalf("acceptable = %d, want 4", got)
	}
	if got := r.entropyBits(); math.Abs(got-2) > 1e-12 {
		t.Fatalf("entropy = %v, want 2", got)
	}
	if got := r.acceptRate(); math.Abs(got-4.0/9) > 1e-12 {
		t.Fatalf("accept rate = %v, want 4/9", got)
	}
	// Four disjoint classes at length 4: 4!·26·26·10·|symbols| strings.
	o := genOptions{length: 4}
	o.symbols = genSymbols
	rr, err := o.recipe()
	if err != nil {
		t.Fatal(err)
	}
	want := 24 * 26 * 26 * 10 * int64(len(runeSet(genSymbols, false)))
	if got := rr.acceptable().Int64(); got != want {
		t.Fatalf("acceptable = %d, want %d", got, want)
	}
	// Overlapping classes must not be double-counted: {a,b} and {b,c},
	// length 1: no single character is in both → 0... except b, which is.
	r = &genRecipe{length: 1, alphabet: []rune("abc"), required: [][]rune{[]rune("ab"), []rune("bc")}}
	if got := r.acceptable().Int64(); got != 1 {
		t.Fatalf("overlapping classes: acceptable = %d, want 1 (just b)", got)
	}
	// Impossible: more classes than characters.
	r = &genRecipe{length: 1, alphabet: []rune("ab"), required: [][]rune{[]rune("a"), []rune("b")}}
	if got := r.acceptable().Sign(); got != 0 {
		t.Fatalf("impossible recipe: acceptable sign = %d, want 0", got)
	}
}

// Every random byte comes from randReader and nothing else; the count of
// reads matches the work (one rand.Int per character, no shortcut path).
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.n++
	return c.r.Read(p)
}

func TestGenReadsOnlyTheInjectedSource(t *testing.T) {
	saved := randReader
	cr := &countingReader{r: saved}
	randReader = cr
	t.Cleanup(func() { randReader = saved })

	if _, _, err := runGenCmd(t, "--charset", "ab", "-l", "16"); err != nil {
		t.Fatal(err)
	}
	if cr.n < 16 {
		t.Fatalf("only %d reads for 16 characters — a character was not drawn from the CSPRNG", cr.n)
	}
}

// The generator must never fall back to a non-cryptographic source.
func TestGenImportsNoMathRand(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"math/rand"`, `"math/rand/v2"`, "time.Now().Unix"} {
		if bytes.Contains(src, []byte(bad)) {
			t.Fatalf("gen.go must not use %s", bad)
		}
	}
	if !bytes.Contains(src, []byte(`"crypto/rand"`)) {
		t.Fatal("gen.go must draw from crypto/rand")
	}
}

// gen is local-only: no token, no URL, no cache file, no network.
func TestGenNeverTouchesVaultOrCache(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("BASHY_SECRETS_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("BASHY_CLOUDBOX_URL", "http://127.0.0.1:1") // nothing listens; a resolve would fail loudly
	cmd := newSecretsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"gen"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gen resolved something it must not: %v", err)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Fatalf("gen wrote into the cache dir: %v", entries)
	}
}

// Uniformity sanity over a two-letter alphabet: a chi-square this loose
// only catches a broken source (e.g. a stuck index), never flakes.
func TestGenRoughlyUniform(t *testing.T) {
	out, _, err := runGenCmd(t, "--charset", "ab", "-l", "1000", "-n", "10")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(lines(out), "")
	a := strings.Count(all, "a")
	if n := len(all); a < n*45/100 || a > n*55/100 {
		t.Fatalf("a appeared %d/%d times — not uniform", a, n)
	}
}

func TestGenVerboseGoesToStderr(t *testing.T) {
	out, errb, err := runGenCmd(t, "-v")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines(out)) != 1 || !strings.Contains(errb, "bits of entropy") {
		t.Fatalf("stdout=%q stderr=%q", out, errb)
	}
}
