package feed

import (
	"fmt"
	"strings"
	"unicode"
)

// Excerpt cap constants.
const (
	HeadlineMax = 90
	DekMax      = 240
	LabelsMax   = 8
	LabelMax    = 32
)

// isWide reports whether r should be weighted with 2 according to
// twitter-text-style rules: CJK (Han, Hiragana, Katakana, Hangul,
// CJK symbols/punctuation U+3000–U+303F) and full-width forms (U+FF00–U+FFEF).
func isWide(r rune) bool {
	if r >= 0x3000 && r <= 0x303F {
		return true
	}
	if r >= 0xFF00 && r <= 0xFFEF {
		return true
	}
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// Weight calculates the twitter-text-style weighted character length of s.
// CJK runes (Han, Hiragana, Katakana, Hangul, CJK symbols/punctuation
// U+3000–U+303F) and full-width forms (U+FF00–U+FFEF) count 2; every other
// rune counts 1.
func Weight(s string) int {
	var total int
	for _, r := range s {
		if isWide(r) {
			total += 2
		} else {
			total += 1
		}
	}
	return total
}

// ValidateExcerpt validates post excerpt fields against platform constraints:
//   - headline: non-empty, one line, ≤90 weighted (HeadlineMax), no trailing period ('.' or '。')
//   - dek: non-empty, ≤240 weighted (DekMax)
//   - labels: ≤8 items (LabelsMax), each non-empty and ≤32 weighted (LabelMax)
//
// When a field exceeds its cap or is empty, it returns an error naming the field,
// its weight (or count), and the cap.
func ValidateExcerpt(headline, dek string, labels []string) error {
	if strings.TrimSpace(headline) == "" {
		return fmt.Errorf("feed: headline is empty (weight %d, cap %d)", Weight(headline), HeadlineMax)
	}
	if strings.ContainsAny(headline, "\r\n") {
		return fmt.Errorf("feed: headline must be a single line")
	}
	if strings.HasSuffix(headline, ".") || strings.HasSuffix(headline, "。") {
		return fmt.Errorf("feed: headline must not end with a period")
	}
	if hw := Weight(headline); hw > HeadlineMax {
		return fmt.Errorf("feed: headline weight %d exceeds cap %d", hw, HeadlineMax)
	}

	if strings.TrimSpace(dek) == "" {
		return fmt.Errorf("feed: dek is empty (weight %d, cap %d)", Weight(dek), DekMax)
	}
	if dw := Weight(dek); dw > DekMax {
		return fmt.Errorf("feed: dek weight %d exceeds cap %d", dw, DekMax)
	}

	if len(labels) > LabelsMax {
		return fmt.Errorf("feed: labels count %d exceeds cap %d", len(labels), LabelsMax)
	}
	for _, l := range labels {
		if strings.TrimSpace(l) == "" {
			return fmt.Errorf("feed: label is empty (weight %d, cap %d)", Weight(l), LabelMax)
		}
		if lw := Weight(l); lw > LabelMax {
			return fmt.Errorf("feed: label %q weight %d exceeds cap %d", l, lw, LabelMax)
		}
	}

	return nil
}
