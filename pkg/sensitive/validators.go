package sensitive

import (
	"sort"
	"strings"
)

// Value kinds. Each is a reason code suffix (value:<kind>) and the label a
// masked span carries ([guarded:<kind>]).
const (
	KindSSN  = "ssn"
	KindITIN = "itin"
	KindEIN  = "ein"
	KindABA  = "aba"
	KindCard = "luhn"
	KindIBAN = "iban"
)

// Span is one guarded value found in a text: [Start, End) byte offsets and its
// kind. It never carries the matched text.
type Span struct {
	Start, End int
	Kind       string
}

// CheckValue reports the kind of guarded value the WHOLE string is, or
// contains, or "" when it is clean. A typed value is checked whole first (a
// bare 9-digit ABA routing number is only recognised as the entire value or
// a bounded token), then scanned for embedded tokens ("my SSN is …").
func CheckValue(s string) string {
	if spans := Scan(s); len(spans) > 0 {
		return spans[0].Kind
	}
	return ""
}

// Scan finds every validator-confirmed guarded value in s, ordered by
// position, overlaps resolved in favour of the earliest, longest span.
//
// The shapes are deliberately strict so ordinary page numbers pass through:
//   - SSN / ITIN: ddd-dd-dddd or ddd dd dddd (one separator kind) with the
//     SSA shape rules (area not 000/666, group not 00, serial not 0000); an
//     area of 9xx is an ITIN only when its group is in the IRS ITIN ranges.
//     A bare 9-digit run is NOT taken as an SSN — it is far likelier to be an
//     order number; it is still checked as an ABA routing number.
//   - EIN: dd-ddddddd with an IRS-assigned campus prefix.
//   - ABA routing: exactly 9 digits bounded by non-digits, a Federal Reserve
//     prefix, and the ABA 3-7-1 checksum.
//   - Card: 13–19 digits (single spaces/dashes allowed between digits), the
//     Luhn checksum AND a known issuer prefix whose length matches.
//   - IBAN: country code with its registered length, then ISO 13616 mod-97.
func Scan(s string) []Span {
	var spans []Span
	spans = append(spans, scanSSN(s)...)
	spans = append(spans, scanEIN(s)...)
	spans = append(spans, scanCards(s)...)
	spans = append(spans, scanABA(s)...)
	spans = append(spans, scanIBAN(s)...)
	return ResolveOverlaps(spans)
}

// ResolveOverlaps orders spans by position and drops any that overlap an
// earlier (or, at the same start, longer) one. Callers that merge spans from
// another matcher (the browser guard's vault) re-resolve with it.
func ResolveOverlaps(spans []Span) []Span {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].End > spans[j].End
	})
	out := spans[:1]
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp.Start < last.End {
			if sp.End > last.End {
				last.End = sp.End
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isAlnum(c byte) bool {
	return isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// digitAt reports whether s[i] exists and is a digit.
func digitAt(s string, i int) bool { return i >= 0 && i < len(s) && isDigit(s[i]) }

// boundedLeft: the byte before i is not a digit, and not a separator glued
// to a digit (so "1-123-45-6789" is not read from its middle).
func boundedLeft(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	if isDigit(c) {
		return false
	}
	if (c == '-' || c == ' ' || c == '.' || c == '/') && digitAt(s, i-2) {
		return false
	}
	return true
}

func boundedRight(s string, j int) bool {
	if j >= len(s) {
		return true
	}
	c := s[j]
	if isDigit(c) {
		return false
	}
	if (c == '-' || c == ' ' || c == '.' || c == '/') && digitAt(s, j+1) {
		return false
	}
	return true
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

// digitsRun reports whether s[i:i+n] is all digits.
func digitsRun(s string, i, n int) bool {
	if i < 0 || i+n > len(s) {
		return false
	}
	for k := i; k < i+n; k++ {
		if !isDigit(s[k]) {
			return false
		}
	}
	return true
}

// --- SSN / ITIN ---------------------------------------------------------

func scanSSN(s string) []Span {
	var out []Span
	for i := 0; i+11 <= len(s); i++ {
		if !digitsRun(s, i, 3) || !boundedLeft(s, i) {
			continue
		}
		sep := s[i+3]
		if sep != '-' && sep != ' ' {
			continue
		}
		if !digitsRun(s, i+4, 2) || s[i+6] != sep || !digitsRun(s, i+7, 4) || !boundedRight(s, i+11) {
			continue
		}
		if kind := ssnKind(s[i:i+3], s[i+4:i+6], s[i+7:i+11]); kind != "" {
			out = append(out, Span{Start: i, End: i + 11, Kind: kind})
		}
	}
	return out
}

// ssnKind applies the SSA/IRS shape rules to the three groups.
func ssnKind(area, group, serial string) string {
	a, g, sr := atoi(area), atoi(group), atoi(serial)
	if g == 0 || sr == 0 {
		return ""
	}
	if a >= 900 {
		// ITIN: 9xx with group 50–65, 70–88, 90–92, 94–99.
		if (g >= 50 && g <= 65) || (g >= 70 && g <= 88) || (g >= 90 && g <= 92) || (g >= 94 && g <= 99) {
			return KindITIN
		}
		return ""
	}
	if a == 0 || a == 666 {
		return ""
	}
	return KindSSN
}

// ValidSSN reports whether s (ddd-dd-dddd, ddd dd dddd or 9 digits) has
// the SSN shape. Exposed for the validator fixtures.
func ValidSSN(s string) bool {
	d := onlyDigits(s)
	return len(d) == 9 && ssnKind(d[:3], d[3:5], d[5:]) == KindSSN
}

// ValidITIN reports whether s has the ITIN shape.
func ValidITIN(s string) bool {
	d := onlyDigits(s)
	return len(d) == 9 && ssnKind(d[:3], d[3:5], d[5:]) == KindITIN
}

// --- EIN ----------------------------------------------------------------

// einPrefixes are the IRS campus-assigned EIN prefixes.
var einPrefixes = func() map[int]bool {
	m := map[int]bool{}
	ranges := [][2]int{{1, 6}, {10, 16}, {20, 27}, {30, 48}, {50, 68}, {71, 77}, {80, 88}, {90, 95}, {98, 99}}
	for _, r := range ranges {
		for p := r[0]; p <= r[1]; p++ {
			m[p] = true
		}
	}
	return m
}()

func scanEIN(s string) []Span {
	var out []Span
	for i := 0; i+10 <= len(s); i++ {
		if !digitsRun(s, i, 2) || !boundedLeft(s, i) || s[i+2] != '-' || !digitsRun(s, i+3, 7) || !boundedRight(s, i+10) {
			continue
		}
		if einPrefixes[atoi(s[i:i+2])] {
			out = append(out, Span{Start: i, End: i + 10, Kind: KindEIN})
		}
	}
	return out
}

// ValidEIN reports whether s is dd-ddddddd with an IRS prefix.
func ValidEIN(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) == 10 && len(scanEIN(s)) == 1
}

// --- ABA routing --------------------------------------------------------

func scanABA(s string) []Span {
	var out []Span
	for i := 0; i+9 <= len(s); i++ {
		if !digitsRun(s, i, 9) || (i > 0 && isAlnum(s[i-1])) || (i+9 < len(s) && isAlnum(s[i+9])) {
			continue
		}
		if ValidABA(s[i : i+9]) {
			out = append(out, Span{Start: i, End: i + 9, Kind: KindABA})
		}
	}
	return out
}

// ValidABA checks a 9-digit routing number: a Federal Reserve prefix
// (00–12 government/banks, 21–32 thrifts, 61–72 electronic, 80 traveller's
// cheques) and the 3-7-1 weighted checksum.
func ValidABA(s string) bool {
	if len(s) != 9 || !digitsRun(s, 0, 9) || s == "000000000" {
		return false
	}
	p := atoi(s[:2])
	if !((p <= 12) || (p >= 21 && p <= 32) || (p >= 61 && p <= 72) || p == 80) {
		return false
	}
	d := func(k int) int { return int(s[k] - '0') }
	sum := 3*(d(0)+d(3)+d(6)) + 7*(d(1)+d(4)+d(7)) + (d(2) + d(5) + d(8))
	return sum%10 == 0
}

// --- payment cards ------------------------------------------------------

func scanCards(s string) []Span {
	var out []Span
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) || !boundedLeft(s, i) {
			continue
		}
		// Greedily read digits with single separators between them.
		j, n := i, 0
		var digits []byte
		end := i
		for j < len(s) && n < 19 {
			if isDigit(s[j]) {
				digits = append(digits, s[j])
				n++
				j++
				end = j
				continue
			}
			if (s[j] == ' ' || s[j] == '-') && digitAt(s, j+1) && n > 0 {
				j++
				continue
			}
			break
		}
		if n < 13 || !boundedRight(s, end) {
			continue
		}
		if ValidCard(string(digits)) {
			out = append(out, Span{Start: i, End: end, Kind: KindCard})
			i = end - 1
		}
	}
	return out
}

// ValidCard reports whether s (digits, optionally separated) is a card
// number: 13–19 digits, a known issuer prefix of matching length, Luhn.
func ValidCard(s string) bool {
	d := onlyDigits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	return issuerMatches(d) && Luhn(d)
}

// Luhn reports whether the digit string passes the Luhn mod-10 check.
func Luhn(d string) bool {
	if len(d) == 0 {
		return false
	}
	sum, double := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		c := d[i]
		if !isDigit(c) {
			return false
		}
		v := int(c - '0')
		if double {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		double = !double
	}
	return sum%10 == 0
}

func issuerMatches(d string) bool {
	n := len(d)
	pre := func(k int) int { return atoi(d[:k]) }
	switch {
	case d[0] == '4': // Visa
		return n == 13 || n == 16 || n == 19
	case pre(2) >= 51 && pre(2) <= 55, pre(4) >= 2221 && pre(4) <= 2720: // Mastercard
		return n == 16
	case pre(2) == 34 || pre(2) == 37: // Amex
		return n == 15
	case pre(4) == 6011 || pre(2) == 65 || (pre(3) >= 644 && pre(3) <= 649): // Discover
		return n >= 16 && n <= 19
	case pre(4) >= 3528 && pre(4) <= 3589: // JCB
		return n >= 16 && n <= 19
	case (pre(3) >= 300 && pre(3) <= 305) || pre(2) == 36 || pre(2) == 38 || pre(2) == 39: // Diners
		return n >= 14 && n <= 19
	case pre(2) == 62: // UnionPay
		return n >= 16 && n <= 19
	}
	return false
}

// --- IBAN ---------------------------------------------------------------

// ibanLengths is the ISO 13616 registry length per country.
var ibanLengths = map[string]int{
	"AD": 24, "AE": 23, "AL": 28, "AT": 20, "AZ": 28, "BA": 20, "BE": 16, "BG": 22, "BH": 22, "BR": 29,
	"BY": 28, "CH": 21, "CR": 22, "CY": 28, "CZ": 24, "DE": 22, "DK": 18, "DO": 28, "EE": 20, "EG": 29,
	"ES": 24, "FI": 18, "FO": 18, "FR": 27, "GB": 22, "GE": 22, "GI": 23, "GL": 18, "GR": 27, "GT": 28,
	"HR": 21, "HU": 28, "IE": 22, "IL": 23, "IQ": 23, "IS": 26, "IT": 27, "JO": 30, "KW": 30, "KZ": 20,
	"LB": 28, "LC": 32, "LI": 21, "LT": 20, "LU": 20, "LV": 21, "MC": 27, "MD": 24, "ME": 22, "MK": 19,
	"MR": 27, "MT": 31, "MU": 30, "NL": 18, "NO": 15, "PK": 24, "PL": 28, "PS": 29, "PT": 25, "QA": 29,
	"RO": 24, "RS": 22, "SA": 24, "SC": 31, "SE": 24, "SI": 19, "SK": 24, "SM": 27, "ST": 25, "SV": 28,
	"TL": 23, "TN": 24, "TR": 26, "UA": 29, "VA": 22, "VG": 24, "XK": 20,
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

func scanIBAN(s string) []Span {
	var out []Span
	for i := 0; i+15 <= len(s); i++ {
		if i > 0 && isAlnum(s[i-1]) {
			continue
		}
		cc := string([]byte{upper(s[i]), upper(s[i+1])})
		want, ok := ibanLengths[cc]
		if !ok || !digitsRun(s, i+2, 2) {
			continue
		}
		// Read exactly `want` alphanumerics, allowing single spaces.
		var b []byte
		j := i
		for j < len(s) && len(b) < want {
			c := s[j]
			if isAlnum(c) {
				b = append(b, upper(c))
				j++
				continue
			}
			if c == ' ' && j+1 < len(s) && isAlnum(s[j+1]) && len(b) > 0 {
				j++
				continue
			}
			break
		}
		if len(b) != want || (j < len(s) && isAlnum(s[j])) {
			continue
		}
		if ibanMod97(string(b)) {
			out = append(out, Span{Start: i, End: j, Kind: KindIBAN})
			i = j - 1
		}
	}
	return out
}

// ValidIBAN reports whether s (spaces allowed) is a registered-length IBAN
// passing mod-97.
func ValidIBAN(s string) bool {
	c := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	if len(c) < 15 {
		return false
	}
	if want, ok := ibanLengths[c[:2]]; !ok || len(c) != want {
		return false
	}
	return ibanMod97(c)
}

func ibanMod97(c string) bool {
	if len(c) < 5 || !digitsRun(c, 2, 2) {
		return false
	}
	r := c[4:] + c[:4]
	rem := 0
	for i := 0; i < len(r); i++ {
		ch := r[i]
		switch {
		case isDigit(ch):
			rem = (rem*10 + int(ch-'0')) % 97
		case ch >= 'A' && ch <= 'Z':
			v := int(ch-'A') + 10
			rem = (rem*100 + v) % 97
		default:
			return false
		}
	}
	return rem == 1
}

func onlyDigits(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if isDigit(s[i]) {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
