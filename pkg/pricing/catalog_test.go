package pricing

import (
	"math"
	"testing"
	"time"
)

func utc(hour int) time.Time { return time.Date(2025, time.July, 20, hour, 0, 0, 0, time.UTC) }

func TestDeepSeekPeakWindowsAndBoundaries(t *testing.T) {
	tests := []struct {
		hour int
		want float64
	}{
		{0, 0.00000027}, {1, 0.00000054}, {3, 0.00000054}, {4, 0.00000027},
		{5, 0.00000027}, {6, 0.00000054}, {9, 0.00000054}, {10, 0.00000027},
	}
	for _, test := range tests {
		got, ok := Default.Rate("deepseek-chat", "input", utc(test.hour))
		if !ok || got != test.want {
			t.Errorf("hour %d: rate = %.8f, known=%v; want %.8f", test.hour, got, ok, test.want)
		}
	}
}

func TestDeepSeekPeakFactorAppliesToAllBillingItems(t *testing.T) {
	for _, item := range []string{"input", "output", "cache_read"} {
		offPeak, _ := Default.Rate("deepseek-v4-pro", item, utc(0))
		peak, _ := Default.Rate("deepseek-v4-pro", item, utc(6))
		if peak != offPeak*2 {
			t.Errorf("%s peak rate = %.8f, want 2x off-peak %.8f", item, peak, offPeak)
		}
	}
}

func TestFlatRateModelIsUnaffected(t *testing.T) {
	offPeak, ok := Default.Rate("example-flat", "output", utc(0))
	peak, peakOK := Default.Rate("example-flat", "output", utc(6))
	if !ok || !peakOK || offPeak != peak {
		t.Fatalf("flat rate changed: off-peak %.8f (%v), peak %.8f (%v)", offPeak, ok, peak, peakOK)
	}
}

func TestAccrualAcrossPeakBoundary(t *testing.T) {
	a := Accrual{Table: Default}
	if total, ok := a.AddTurn("deepseek-chat", map[string]int64{"input": 100}, utc(0)); !ok || !closeEnough(total, 0.000027) {
		t.Fatalf("off-peak total = %.8f, known=%v", total, ok)
	}
	if total, ok := a.AddTurn("deepseek-chat", map[string]int64{"input": 100}, utc(1)); !ok || !closeEnough(total, 0.000081) {
		t.Fatalf("cross-boundary total = %.8f, known=%v; want .000081", total, ok)
	}
}

func TestRateUsesExplicitUTCInstant(t *testing.T) {
	instant := time.Date(2025, time.July, 20, 1, 30, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	got, ok := Default.Rate("deepseek-chat", "input", instant)
	if !ok || got != 0.00000027 { // 17:30 UTC, outside the peak windows
		t.Fatalf("rate = %.8f, known=%v; want off-peak", got, ok)
	}
	again, againOK := Default.Rate("deepseek-chat", "input", instant)
	if !againOK || again != got {
		t.Fatalf("same explicit instant was not deterministic: %.8f then %.8f", got, again)
	}
}

func TestExplicitPeakItemRate(t *testing.T) {
	table := Table{Models: map[string]Model{"example": {
		Items:       map[string]float64{"input": 1},
		PeakWindows: []HourRange{{Start: 1, End: 4}},
		PeakItems:   map[string]float64{"input": 3},
	}}}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	got, ok := table.Rate("example", "input", utc(1))
	if !ok || got != 3 {
		t.Fatalf("explicit peak rate = %v, known=%v; want 3", got, ok)
	}
}

func closeEnough(got, want float64) bool { return math.Abs(got-want) < 1e-15 }
