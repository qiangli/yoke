package guard

import (
	"strings"
	"testing"
)

// Red/green per data type: every value below MUST be caught (red), every
// near-miss MUST pass (green). The near-misses are the shapes a form
// legitimately asks for — order numbers, phone numbers, dates, ZIP codes,
// amounts — which the guard must never block.

func TestValidatorsCatchEachDataType(t *testing.T) {
	red := map[string]string{
		"212-48-7731":                       KindSSN,
		"212 48 7731":                       KindSSN,
		"my ssn is 212-48-7731.":            KindSSN,
		"912-70-1234":                       KindITIN,
		"12-3456789":                        KindEIN,
		"EIN: 94-1234567":                   KindEIN,
		"021000021":                         KindABA, // JPMorgan Chase NY routing
		"routing 011000015":                 KindABA, // Federal Reserve Boston
		"4111111111111111":                  KindCard,
		"4111 1111 1111 1111":               KindCard,
		"5500-0000-0000-0004":               KindCard,
		"378282246310005":                   KindCard, // Amex test number
		"6011111111111117":                  KindCard,
		"DE89370400440532013000":            KindIBAN,
		"GB82 WEST 1234 5698 7654 32":       KindIBAN,
		"iban: fr1420041010050500013m02606": KindIBAN,
	}
	for in, want := range red {
		if got := CheckValue(in); got != want {
			t.Errorf("CheckValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidatorsPassOrdinaryNumbers(t *testing.T) {
	green := []string{
		// order numbers
		"ORD-2026-001234", "#100234", "Order 7734120098", "A1B2C3D4E5", "INV-55-123456",
		// phone numbers
		"(415) 555-0132", "+1 415 555 0132", "415-555-0132", "+44 20 7946 0958", "1-800-555-0199",
		// dates and times
		"2026-10-28", "10/28/2026", "28.10.2026", "Oct 28, 2026 14:30", "2026-10-28T14:30:00Z",
		// ZIP / postal codes
		"94105", "94105-1234", "SW1A 1AA", "10115",
		// amounts and quantities
		"$1,234.56", "€ 99,00", "1234567.89", "qty 12",
		// SSN-shaped but invalid by SSA rules
		"000-12-3456", "666-12-3456", "123-00-4567", "123-45-0000", "987-12-4321",
		// card-length but failing Luhn or issuer
		"4111111111111112", "1234567812345678",
		// 9 digits failing the ABA checksum
		"123456789",
		// IBAN-shaped with a bad check
		"DE89370400440532013001",
		"Booking ref 2026102812345",
	}
	for _, in := range green {
		if got := CheckValue(in); got != "" {
			t.Errorf("CheckValue(%q) = %q, want clean (false positive)", in, got)
		}
	}
}

func TestValidatorPrimitives(t *testing.T) {
	if !ValidSSN("212-48-7731") || ValidSSN("666-48-7731") || ValidSSN("912-70-1234") {
		t.Error("ValidSSN shape rules wrong")
	}
	if !ValidITIN("912-70-1234") || ValidITIN("912-40-1234") {
		t.Error("ValidITIN group ranges wrong")
	}
	if !ValidEIN("12-3456789") || ValidEIN("07-3456789") || ValidEIN("00-1234567") {
		t.Error("ValidEIN prefixes wrong")
	}
	if !ValidABA("021000021") || ValidABA("021000022") || ValidABA("000000000") || ValidABA("990000021") {
		t.Error("ValidABA wrong")
	}
	if !Luhn("79927398713") || Luhn("79927398710") {
		t.Error("Luhn wrong")
	}
	if !ValidCard("4111-1111-1111-1111") || ValidCard("4111-1111-1111") {
		t.Error("ValidCard wrong")
	}
	if !ValidIBAN("GB82 WEST 1234 5698 7654 32") || ValidIBAN("GB82 WEST 1234 5698 7654 33") || ValidIBAN("ZZ82WEST12345698765432") {
		t.Error("ValidIBAN wrong")
	}
}

func TestScanFindsEveryValueInText(t *testing.T) {
	text := "Name: Ada. SSN 212-48-7731, card 4111 1111 1111 1111, phone 415-555-0132, IBAN DE89370400440532013000."
	spans := Scan(text)
	var kinds []string
	for _, sp := range spans {
		kinds = append(kinds, sp.Kind)
	}
	if got := strings.Join(kinds, ","); got != "ssn,luhn,iban" {
		t.Fatalf("Scan kinds = %s, want ssn,luhn,iban", got)
	}
}
