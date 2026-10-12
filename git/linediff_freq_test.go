package git

import (
	"fmt"
	osexec "os/exec"
	"strings"
	"testing"
)

// freqCases builds the frequent-line probe families as (A, B) pairs:
// the discard rules of compareDiscarding at their boundaries.
func freqCases() [][2][]string {
	var out [][2][]string
	u := func(p string, n int) []string {
		var s []string
		for i := 0; i < n; i++ {
			s = append(s, fmt.Sprintf("%s%d", p, i))
		}
		return s
	}
	cat := func(parts ...[]string) []string {
		var s []string
		for _, p := range parts {
			s = append(s, p...)
		}
		return s
	}
	rep := func(x string, n int) []string {
		return cat(make([]string, 0), func() []string {
			r := make([]string, n)
			for i := range r {
				r[i] = x
			}
			return r
		}())
	}
	Z, Y := u("z", 40), u("y", 40)
	// family 1: p-run of frequent blanks flanked by unique lines (mlim4)
	for p := 1; p <= 3; p++ {
		for bef := 0; bef <= 8; bef++ {
			for aft := 0; aft <= 10; aft++ {
				out = append(out, [2][]string{cat(Z, u("u", bef), rep("", p), u("v", aft), Y), cat(Z, rep("", 30), Y)})
			}
		}
	}
	// family 2: F,F' separated (mlim6)
	for _, b := range []int{1, 2, 3, 5} {
		for a := 0; a <= 10; a += 2 {
			for c := 0; c <= 10; c += 2 {
				out = append(out, [2][]string{cat(Z, u("u", a), []string{"F"}, u("v", b), []string{"F"}, u("w", c), Y), cat(Z, rep("F", 30), Y)})
			}
		}
	}
	// family 3: y in B between unique runs, optional prefix (fr1/fr2)
	for _, pre := range [][]string{nil, {"x"}, {"o", "x"}, {"o"}} {
		for u0 := 1; u0 <= 8; u0++ {
			for u1 := 1; u1 <= 8; u1++ {
				b := cat(pre, u("u", u0), []string{"y"}, u("v", u1))
				a := cat(rep("o", 1), rep("x", 40), rep("y", 40))
				out = append(out, [2][]string{a, b})
				out = append(out, [2][]string{rep("y", 4), cat(u("u", u0), []string{"y"}, u("v", u1))})
			}
		}
	}
	// family 4: frequent blanks spread through long unique runs, where
	// the scans' 100-line cap decides (UFF + (UUUF)*k + U*t against
	// blanks only)
	for _, k := range []int{2, 8, 20, 24, 28, 30} {
		for _, t := range []int{0, 6, 10, 20} {
			p := "UFF" + strings.Repeat("UUUF", k) + strings.Repeat("U", t)
			var a []string
			for i, ch := range p {
				if ch == 'U' {
					a = append(a, fmt.Sprintf("u%d", i))
				} else {
					a = append(a, "")
				}
			}
			out = append(out, [2][]string{a, rep("", 64)})
		}
	}
	return out
}

// TestLineDiffFrequentLines checks the discard rules against host git on
// probe families built around their boundaries.
func TestLineDiffFrequentLines(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("no host git on PATH")
	}
	if testing.Short() {
		t.Skip("slow")
	}
	dir := t.TempDir()
	bad := 0
	cs := freqCases()
	for i, c := range cs {
		a, b := joinLines(c[0], true), joinLines(c[1], true)
		if nativeHunks(a, b, false) != hostHunks(t, dir, a, b, false) {
			bad++
			if bad <= 3 {
				t.Errorf("case %d differs: A=%q B=%q", i, c[0], c[1])
			}
		}
	}
	t.Logf("frequent-line probes: %d/%d identical", len(cs)-bad, len(cs))
}
