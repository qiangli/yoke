package git

import (
	"math/rand"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// oracleVocab mixes code-like lines whose repeats create the ambiguous
// slides (closing braces, blank lines, repeated statements) that decide
// whether hunk placement matches host git.
var oracleVocab = []string{
	"", "", "", "}", "}", "\t}", "\t}", "\t\t}", "{", "func a() {", "func b() {",
	"\treturn nil", "\treturn err", "\tif err != nil {", "\t\treturn err",
	"\tx := 1", "\ty := 2", "\t// comment", "// doc", "import (", ")", "end",
	"line a", "line b", "line c", "    indented", "        deeper",
}

func oracleFile(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = oracleVocab[r.Intn(len(oracleVocab))]
	}
	return out
}

func oracleMutate(r *rand.Rand, base []string) []string {
	out := append([]string(nil), base...)
	for e := r.Intn(4) + 1; e > 0; e-- {
		switch op := r.Intn(4); {
		case op == 0 && len(out) > 0: // delete a run
			i := r.Intn(len(out))
			k := min(r.Intn(4)+1, len(out)-i)
			out = append(out[:i], out[i+k:]...)
		case op == 1: // insert a run
			i := r.Intn(len(out) + 1)
			ins := oracleFile(r, r.Intn(5)+1)
			out = append(out[:i], append(ins, out[i:]...)...)
		case op == 2 && len(out) > 0: // replace a line
			out[r.Intn(len(out))] = oracleVocab[r.Intn(len(oracleVocab))]
		default: // duplicate a block (classic slider)
			if len(out) > 2 {
				i := r.Intn(len(out) - 1)
				k := min(r.Intn(3)+1, len(out)-i)
				blk := append([]string(nil), out[i:i+k]...)
				out = append(out[:i+k], append(blk, out[i+k:]...)...)
			}
		}
	}
	return out
}

func joinLines(ls []string, finalNL bool) string {
	s := strings.Join(ls, "\n")
	if finalNL && len(ls) > 0 {
		s += "\n"
	}
	return s
}

// hostHunks is `git diff --no-index` from the first hunk header on.
func hostHunks(t *testing.T, dir, a, b string, indent bool) string {
	t.Helper()
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(pa, []byte(a), 0o644)
	os.WriteFile(pb, []byte(b), 0o644)
	args := []string{"-c", "core.quotepath=off", "diff", "--no-index", "--no-color", "--no-ext-diff"}
	if !indent {
		args = append(args, "--no-indent-heuristic")
	}
	out, _ := osexec.Command("git", append(args, pa, pb)...).Output()
	s := string(out)
	if i := strings.Index(s, "\n@@ "); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func nativeHunks(a, b string, indent bool) string {
	la, lb := diffSplitLines(a), diffSplitLines(b)
	return newLineDiff(la, lb, indent).unified(3, funcNamer(la))
}

// TestLineDiffOracle compares native hunks with host git's on random
// code-like edits. LINEDIFF_ORACLE_N raises the case count; the default
// keeps the suite fast.
func TestLineDiffOracle(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
	n := 300
	if v, err := strconv.Atoi(os.Getenv("LINEDIFF_ORACLE_N")); err == nil {
		n = v
	}
	seed := int64(1)
	if v, err := strconv.ParseInt(os.Getenv("LINEDIFF_ORACLE_SEED"), 10, 64); err == nil {
		seed = v
	}
	dir := t.TempDir()
	for _, indent := range []bool{false, true} {
		r := rand.New(rand.NewSource(seed))
		bad := 0
		for c := 0; c < n; c++ {
			base := oracleFile(r, r.Intn(30)+1)
			a := joinLines(base, r.Intn(8) != 0)
			b := joinLines(oracleMutate(r, base), r.Intn(8) != 0)
			want := hostHunks(t, dir, a, b, indent)
			got := nativeHunks(a, b, indent)
			if got != want {
				bad++
				if bad <= 3 {
					t.Errorf("indent=%v case %d differs\n--- a ---\n%q\n--- b ---\n%q\n--- native ---\n%s--- host ---\n%s", indent, c, a, b, got, want)
				}
			}
		}
		t.Logf("indent=%v: %d/%d identical", indent, n-bad, n)
	}
}

// TestLineDiffOracleExpensive covers host git's bounded search: large,
// heavily edited files over a small alphabet make the middle-snake
// search hit searchCostLimit, where host git (without --minimal) gives
// up and splits early. Native patches must match it exactly.
func TestLineDiffOracleExpensive(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
	if testing.Short() {
		t.Skip("slow")
	}
	dir := t.TempDir()
	r := rand.New(rand.NewSource(11))
	for c := 0; c < 12; c++ {
		alpha := 2 + r.Intn(8)
		rate := r.Float64()
		var la, lb []string
		for i, n := 0, 300+r.Intn(1500); i < n; i++ {
			la = append(la, strconv.Itoa(r.Intn(alpha)))
		}
		for _, l := range la {
			if r.Float64() < rate {
				if r.Intn(2) == 0 {
					continue
				}
				lb = append(lb, strconv.Itoa(r.Intn(alpha)))
			}
			lb = append(lb, l)
		}
		for _, indent := range []bool{false, true} {
			a, b := joinLines(la, true), joinLines(lb, true)
			if got, want := nativeHunks(a, b, indent), hostHunks(t, dir, a, b, indent); got != want {
				t.Errorf("case %d (indent=%v, %d/%d lines) differs from host git", c, indent, len(la), len(lb))
			}
		}
	}
}

// TestLineDiffDeclinesLargeGiveUp: a search that gives up on a box
// larger than giveUpVerifiedSize is declined (callers defer to host
// git); a smaller expensive diff and a large cheap one are served.
func TestLineDiffDeclinesLargeGiveUp(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	gen := func(n int, rate float64) ([]string, []string) {
		var a, b []string
		for i := 0; i < n; i++ {
			a = append(a, strconv.Itoa(r.Intn(4))+"\n")
		}
		for _, l := range a {
			if r.Float64() < rate {
				if r.Intn(2) == 0 {
					continue
				}
				b = append(b, strconv.Itoa(r.Intn(4))+"\n")
			}
			b = append(b, l)
		}
		return a, b
	}
	for _, c := range []struct {
		n    int
		rate float64
		want bool
	}{{2000, 0.3, false}, {40000, 0.3, true}, {40000, 0.001, false}} {
		a, b := gen(c.n, c.rate)
		if got := newLineDiff(a, b, false).declined; got != c.want {
			t.Errorf("n=%d rate=%v: declined=%v, want %v", c.n, c.rate, got, c.want)
		}
	}
}
