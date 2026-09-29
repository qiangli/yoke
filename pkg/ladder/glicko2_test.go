package ladder

import (
	"math"
	"testing"
)

func TestGlickoWorkedExample(t *testing.T) {
	r := Update(Rating{R: 1500, RD: 200, Vol: 0.06}, []Result{
		{Opponent: Rating{R: 1400, RD: 30}, Score: 1},
		{Opponent: Rating{R: 1550, RD: 100}, Score: 0},
		{Opponent: Rating{R: 1700, RD: 300}, Score: 0},
	}, 0.5)
	if math.Abs(r.R-1464.06) > 0.01 || math.Abs(r.RD-151.52) > 0.01 || math.Abs(r.Vol-0.05999) > 1e-5 {
		t.Fatalf("worked example: got %+v, want approximately 1464.06/151.52/0.05999", r)
	}
}

func TestEmptyPeriod(t *testing.T) {
	r := Rating{R: 1500, RD: 200, Vol: 0.06}
	got := Update(r, nil, DefaultTau)
	if got.R != r.R || got.Vol != r.Vol || got.RD <= r.RD || got.RD > MaxRD {
		t.Fatalf("empty period: got %+v", got)
	}
	got = Update(NewRating(), nil, DefaultTau)
	if got.RD != MaxRD {
		t.Fatalf("initial RD exceeded cap: got %+v", got)
	}
}

func TestExpectedEqualRD(t *testing.T) {
	a := Rating{R: 1600, RD: 100}
	b := Rating{R: 1400, RD: 100}
	if got := Expected(a, b) + Expected(b, a); math.Abs(got-1) > 1e-12 {
		t.Fatalf("expected scores sum to %g, want 1", got)
	}
	if got := Expected(a, a); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("self expected score = %g, want 0.5", got)
	}
}

func TestDecayMonotone(t *testing.T) {
	r := Rating{R: 1500, RD: 80, Vol: 0.06}
	prev := r
	for n := 0; n <= 100000; n += 1000 {
		got := Decay(r, n)
		if got.R != r.R || got.Vol != r.Vol || got.RD < prev.RD || got.RD > MaxRD {
			t.Fatalf("decay %d: got %+v after %+v", n, got, prev)
		}
		prev = got
	}
	if got := Decay(r, 2); math.Abs(got.RD-Update(Update(r, nil, DefaultTau), nil, DefaultTau).RD) > 1e-12 {
		t.Fatalf("two periods: got RD %g", got.RD)
	}
}

func TestConservative(t *testing.T) {
	if got := Conservative(Rating{R: 1500, RD: 200}); got != 1100 {
		t.Fatalf("conservative rating = %g, want 1100", got)
	}
}
