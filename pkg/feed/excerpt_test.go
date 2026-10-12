package feed

import (
	"strconv"
	"strings"
	"testing"
)

func TestWeight(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "empty string",
			in:   "",
			want: 0,
		},
		{
			name: "latin single character",
			in:   "x",
			want: 1,
		},
		{
			name: "latin sentence with punctuation and spaces",
			in:   "Hello, world! 123",
			want: 17,
		},
		{
			name: "cjk han characters",
			in:   "中文字符",
			want: 8, // 4 runes * 2
		},
		{
			name: "cjk hiragana",
			in:   "こんにちは",
			want: 10, // 5 runes * 2
		},
		{
			name: "cjk katakana",
			in:   "コンニチハ",
			want: 10, // 5 runes * 2
		},
		{
			name: "cjk hangul",
			in:   "안녕하세요",
			want: 10, // 5 runes * 2
		},
		{
			name: "cjk symbols and punctuation U+3000 to U+303F",
			in:   "\u3000、。〈〉《》「」",
			want: 18, // 9 runes * 2
		},
		{
			name: "full-width forms U+FF00 to U+FFEF",
			in:   "！ＡＢＣ１２３（）",
			want: 18, // 9 runes * 2
		},
		{
			name: "mixed latin, han, hiragana and punctuation",
			in:   "Hello 世界、こんにちは！",
			// "Hello " = 6
			// "世界" = 4
			// "、" = 2
			// "こんにちは" = 10
			// "！" = 2
			// Total = 6 + 4 + 2 + 10 + 2 = 24
			want: 24,
		},
		{
			name: "other scripts and emoji count as 1",
			in:   "café Привет Ελλάδα 🚀🔥",
			// "café " = 5 ('é' is 1)
			// "Привет " = 7 (6 cyrillic + 1 space)
			// "Ελλάδα " = 7 (6 greek + 1 space)
			// "🚀🔥" = 2
			// Total = 5 + 7 + 7 + 2 = 21
			want: 21,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Weight(tt.in)
			if got != tt.want {
				t.Errorf("Weight(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateExcerptHeadline(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	validDek := "Valid dek description for test."
	validLabels := []string{"test"}

	tests := []struct {
		name        string
		headline    string
		wantErr     bool
		errField    string
		wantWeight  int
		wantCap     int
		errContains string
	}{
		// Latin
		{
			name:       "latin exactly at cap",
			headline:   strings.Repeat("a", HeadlineMax),
			wantErr:    false,
			wantWeight: HeadlineMax,
		},
		{
			name:       "latin cap + 1",
			headline:   strings.Repeat("a", HeadlineMax+1),
			wantErr:    true,
			errField:   "headline",
			wantWeight: HeadlineMax + 1,
			wantCap:    HeadlineMax,
		},
		// CJK
		{
			name:       "cjk exactly at cap",
			headline:   strings.Repeat("中", HeadlineMax/2), // 45 * 2 = 90
			wantErr:    false,
			wantWeight: HeadlineMax,
		},
		{
			name:       "cjk cap + 1",
			headline:   strings.Repeat("中", HeadlineMax/2) + "a", // 45 * 2 + 1 = 91
			wantErr:    true,
			errField:   "headline",
			wantWeight: HeadlineMax + 1,
			wantCap:    HeadlineMax,
		},
		// Mixed
		{
			name:       "mixed exactly at cap",
			headline:   strings.Repeat("中", 40) + strings.Repeat("x", 10), // 40*2 + 10 = 90
			wantErr:    false,
			wantWeight: HeadlineMax,
		},
		{
			name:       "mixed cap + 1",
			headline:   strings.Repeat("中", 40) + strings.Repeat("x", 11), // 40*2 + 11 = 91
			wantErr:    true,
			errField:   "headline",
			wantWeight: HeadlineMax + 1,
			wantCap:    HeadlineMax,
		},
		// Full-width
		{
			name:       "full-width exactly at cap",
			headline:   strings.Repeat("Ａ", HeadlineMax/2), // 45 * 2 = 90
			wantErr:    false,
			wantWeight: HeadlineMax,
		},
		{
			name:       "full-width cap + 1",
			headline:   strings.Repeat("Ａ", HeadlineMax/2) + "b", // 45 * 2 + 1 = 91
			wantErr:    true,
			errField:   "headline",
			wantWeight: HeadlineMax + 1,
			wantCap:    HeadlineMax,
		},
		// Other headline constraints
		{
			name:        "empty headline",
			headline:    "",
			wantErr:     true,
			errField:    "headline",
			wantWeight:  0,
			wantCap:     HeadlineMax,
			errContains: "empty",
		},
		{
			name:        "whitespace only headline",
			headline:    "   \t  ",
			wantErr:     true,
			errField:    "headline",
			wantCap:     HeadlineMax,
			errContains: "empty",
		},
		{
			name:        "multi-line headline with newline",
			headline:    "First line\nSecond line",
			wantErr:     true,
			errField:    "headline",
			errContains: "single line",
		},
		{
			name:        "multi-line headline with carriage return",
			headline:    "First line\rSecond line",
			wantErr:     true,
			errField:    "headline",
			errContains: "single line",
		},
		{
			name:        "trailing ascii period",
			headline:    "Fed holds rates steady.",
			wantErr:     true,
			errField:    "headline",
			errContains: "period",
		},
		{
			name:        "trailing cjk period",
			headline:    "央行维持利率不变。",
			wantErr:     true,
			errField:    "headline",
			errContains: "period",
		},
		{
			name:     "period in middle is valid",
			headline: "Release v1.2 is finally out",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.wantErr {
				if w := Weight(tt.headline); w != tt.wantWeight && tt.wantWeight != 0 {
					t.Fatalf("test setup error: Weight(%q) = %d, want %d", tt.headline, w, tt.wantWeight)
				}
			}
			err := ValidateExcerpt(tt.headline, validDek, validLabels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExcerpt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				msg := err.Error()
				if tt.errField != "" && !strings.Contains(msg, tt.errField) {
					t.Errorf("error %q does not name field %q", msg, tt.errField)
				}
				if tt.wantWeight > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantWeight)) {
					t.Errorf("error %q does not contain weight %d", msg, tt.wantWeight)
				}
				if tt.wantCap > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantCap)) {
					t.Errorf("error %q does not contain cap %d", msg, tt.wantCap)
				}
				if tt.errContains != "" && !strings.Contains(msg, tt.errContains) {
					t.Errorf("error %q does not contain %q", msg, tt.errContains)
				}
			}
		})
	}
}

func TestValidateExcerptDek(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	validHeadline := "Valid headline for test"
	validLabels := []string{"test"}

	tests := []struct {
		name        string
		dek         string
		wantErr     bool
		errField    string
		wantWeight  int
		wantCap     int
		errContains string
	}{
		// Latin
		{
			name:       "latin exactly at cap",
			dek:        strings.Repeat("d", DekMax),
			wantErr:    false,
			wantWeight: DekMax,
		},
		{
			name:       "latin cap + 1",
			dek:        strings.Repeat("d", DekMax+1),
			wantErr:    true,
			errField:   "dek",
			wantWeight: DekMax + 1,
			wantCap:    DekMax,
		},
		// CJK
		{
			name:       "cjk exactly at cap",
			dek:        strings.Repeat("述", DekMax/2), // 120 * 2 = 240
			wantErr:    false,
			wantWeight: DekMax,
		},
		{
			name:       "cjk cap + 1",
			dek:        strings.Repeat("述", DekMax/2) + "a", // 120 * 2 + 1 = 241
			wantErr:    true,
			errField:   "dek",
			wantWeight: DekMax + 1,
			wantCap:    DekMax,
		},
		// Mixed
		{
			name:       "mixed exactly at cap",
			dek:        strings.Repeat("述", 100) + strings.Repeat("d", 40), // 100*2 + 40 = 240
			wantErr:    false,
			wantWeight: DekMax,
		},
		{
			name:       "mixed cap + 1",
			dek:        strings.Repeat("述", 100) + strings.Repeat("d", 41), // 100*2 + 41 = 241
			wantErr:    true,
			errField:   "dek",
			wantWeight: DekMax + 1,
			wantCap:    DekMax,
		},
		// Full-width
		{
			name:       "full-width exactly at cap",
			dek:        strings.Repeat("Ｄ", DekMax/2), // 120 * 2 = 240
			wantErr:    false,
			wantWeight: DekMax,
		},
		{
			name:       "full-width cap + 1",
			dek:        strings.Repeat("Ｄ", DekMax/2) + "x", // 120 * 2 + 1 = 241
			wantErr:    true,
			errField:   "dek",
			wantWeight: DekMax + 1,
			wantCap:    DekMax,
		},
		// Dek empty
		{
			name:        "empty dek",
			dek:         "",
			wantErr:     true,
			errField:    "dek",
			wantWeight:  0,
			wantCap:     DekMax,
			errContains: "empty",
		},
		{
			name:        "whitespace only dek",
			dek:         "    \n  \t ",
			wantErr:     true,
			errField:    "dek",
			wantCap:     DekMax,
			errContains: "empty",
		},
		{
			name:    "dek with trailing period is allowed",
			dek:     "Sentences in dek can end in a period.",
			wantErr: false,
		},
		{
			name:    "dek with cjk period is allowed",
			dek:     "描述句子可以以句号结尾。",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.wantErr {
				if w := Weight(tt.dek); w != tt.wantWeight && tt.wantWeight != 0 {
					t.Fatalf("test setup error: Weight(%q) = %d, want %d", tt.dek, w, tt.wantWeight)
				}
			}
			err := ValidateExcerpt(validHeadline, tt.dek, validLabels)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExcerpt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				msg := err.Error()
				if tt.errField != "" && !strings.Contains(msg, tt.errField) {
					t.Errorf("error %q does not name field %q", msg, tt.errField)
				}
				if tt.wantWeight > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantWeight)) {
					t.Errorf("error %q does not contain weight %d", msg, tt.wantWeight)
				}
				if tt.wantCap > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantCap)) {
					t.Errorf("error %q does not contain cap %d", msg, tt.wantCap)
				}
				if tt.errContains != "" && !strings.Contains(msg, tt.errContains) {
					t.Errorf("error %q does not contain %q", msg, tt.errContains)
				}
			}
		})
	}
}

func TestValidateExcerptLabels(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())

	validHeadline := "Valid headline for test"
	validDek := "Valid dek for test."

	// Test label count cap
	t.Run("labels count", func(t *testing.T) {
		// 8 labels: at cap
		labels8 := make([]string, LabelsMax)
		for i := range labels8 {
			labels8[i] = "label" + strconv.Itoa(i)
		}
		if err := ValidateExcerpt(validHeadline, validDek, labels8); err != nil {
			t.Errorf("8 labels failed: %v", err)
		}

		// 9 labels: cap + 1
		labels9 := make([]string, LabelsMax+1)
		for i := range labels9 {
			labels9[i] = "label" + strconv.Itoa(i)
		}
		err := ValidateExcerpt(validHeadline, validDek, labels9)
		if err == nil {
			t.Fatal("9 labels succeeded, want error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "labels") || !strings.Contains(msg, "9") || !strings.Contains(msg, "8") {
			t.Errorf("error %q does not name field 'labels', count 9, cap 8", msg)
		}

		// nil or 0 labels: valid
		if err := ValidateExcerpt(validHeadline, validDek, nil); err != nil {
			t.Errorf("nil labels failed: %v", err)
		}
		if err := ValidateExcerpt(validHeadline, validDek, []string{}); err != nil {
			t.Errorf("empty labels slice failed: %v", err)
		}
	})

	// Test individual label weight caps
	labelTests := []struct {
		name        string
		label       string
		wantErr     bool
		errField    string
		wantWeight  int
		wantCap     int
		errContains string
	}{
		// Latin
		{
			name:       "latin label exactly at cap",
			label:      strings.Repeat("l", LabelMax),
			wantErr:    false,
			wantWeight: LabelMax,
		},
		{
			name:       "latin label cap + 1",
			label:      strings.Repeat("l", LabelMax+1),
			wantErr:    true,
			errField:   "label",
			wantWeight: LabelMax + 1,
			wantCap:    LabelMax,
		},
		// CJK
		{
			name:       "cjk label exactly at cap",
			label:      strings.Repeat("标", LabelMax/2), // 16 * 2 = 32
			wantErr:    false,
			wantWeight: LabelMax,
		},
		{
			name:       "cjk label cap + 1",
			label:      strings.Repeat("标", LabelMax/2) + "a", // 16 * 2 + 1 = 33
			wantErr:    true,
			errField:   "label",
			wantWeight: LabelMax + 1,
			wantCap:    LabelMax,
		},
		// Mixed
		{
			name:       "mixed label exactly at cap",
			label:      strings.Repeat("标", 14) + strings.Repeat("x", 4), // 14*2 + 4 = 32
			wantErr:    false,
			wantWeight: LabelMax,
		},
		{
			name:       "mixed label cap + 1",
			label:      strings.Repeat("标", 14) + strings.Repeat("x", 5), // 14*2 + 5 = 33
			wantErr:    true,
			errField:   "label",
			wantWeight: LabelMax + 1,
			wantCap:    LabelMax,
		},
		// Full-width
		{
			name:       "full-width label exactly at cap",
			label:      strings.Repeat("Ｌ", LabelMax/2), // 16 * 2 = 32
			wantErr:    false,
			wantWeight: LabelMax,
		},
		{
			name:       "full-width label cap + 1",
			label:      strings.Repeat("Ｌ", LabelMax/2) + "y", // 16 * 2 + 1 = 33
			wantErr:    true,
			errField:   "label",
			wantWeight: LabelMax + 1,
			wantCap:    LabelMax,
		},
		// Empty label
		{
			name:        "empty label",
			label:       "",
			wantErr:     true,
			errField:    "label",
			wantWeight:  0,
			wantCap:     LabelMax,
			errContains: "empty",
		},
		{
			name:        "whitespace only label",
			label:       "   ",
			wantErr:     true,
			errField:    "label",
			wantCap:     LabelMax,
			errContains: "empty",
		},
	}

	for _, tt := range labelTests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.wantErr {
				if w := Weight(tt.label); w != tt.wantWeight && tt.wantWeight != 0 {
					t.Fatalf("test setup error: Weight(%q) = %d, want %d", tt.label, w, tt.wantWeight)
				}
			}
			err := ValidateExcerpt(validHeadline, validDek, []string{tt.label})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateExcerpt() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				msg := err.Error()
				if tt.errField != "" && !strings.Contains(msg, tt.errField) {
					t.Errorf("error %q does not name field %q", msg, tt.errField)
				}
				if tt.wantWeight > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantWeight)) {
					t.Errorf("error %q does not contain weight %d", msg, tt.wantWeight)
				}
				if tt.wantCap > 0 && !strings.Contains(msg, strconv.Itoa(tt.wantCap)) {
					t.Errorf("error %q does not contain cap %d", msg, tt.wantCap)
				}
				if tt.errContains != "" && !strings.Contains(msg, tt.errContains) {
					t.Errorf("error %q does not contain %q", msg, tt.errContains)
				}
			}
		})
	}
}
