package romanloop

import "testing"

// NOTE: these two cases are mutually contradictory — ToRoman(4) cannot be both.
func TestToRomanIV(t *testing.T) {
	if got := ToRoman(4); got != "IV" {
		t.Fatalf("ToRoman(4) = %q, want IV", got)
	}
}

func TestToRomanIIII(t *testing.T) {
	if got := ToRoman(4); got != "IIII" {
		t.Fatalf("ToRoman(4) = %q, want IIII", got)
	}
}
