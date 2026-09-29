package ladder

import "math"

const (
	// InitialR, InitialRD, and InitialVol are the starting Glicko-2 rating.
	InitialR   = 1500.0
	InitialRD  = 350.0
	InitialVol = 0.06
	// DefaultTau controls how quickly volatility may change.
	DefaultTau = 0.5
	// MaxRD bounds rating uncertainty on the public scale.
	MaxRD = 350.0

	glickoScale   = 173.7178
	glickoEpsilon = 1e-6
)

// Rating is a Glicko-2 rating on the public rating scale.
type Rating struct {
	R   float64
	RD  float64
	Vol float64
}

// NewRating returns the initial unrated state.
func NewRating() Rating {
	return Rating{R: InitialR, RD: InitialRD, Vol: InitialVol}
}

// Result is one match against an opponent with a score from zero to one.
type Result struct {
	Opponent Rating
	Score    float64
}

// Expected returns a's expected score against b, including b's uncertainty.
func Expected(a, b Rating) float64 {
	muA := (a.R - InitialR) / glickoScale
	muB := (b.R - InitialR) / glickoScale
	phiB := b.RD / glickoScale
	return glickoExpected(muA, muB, phiB)
}

// Update applies one rating period to r. An empty period increases only RD.
func Update(r Rating, results []Result, tau float64) Rating {
	if len(results) == 0 {
		return Decay(r, 1)
	}
	if tau <= 0 {
		tau = DefaultTau
	}

	mu := (r.R - InitialR) / glickoScale
	phi := r.RD / glickoScale
	var inverseV, weightedScore float64
	for _, result := range results {
		oppMu := (result.Opponent.R - InitialR) / glickoScale
		oppPhi := result.Opponent.RD / glickoScale
		g := glickoG(oppPhi)
		e := glickoExpected(mu, oppMu, oppPhi)
		inverseV += g * g * e * (1 - e)
		weightedScore += g * (result.Score - e)
	}
	v := 1 / inverseV
	delta := v * weightedScore
	sigma := glickoVolatility(phi, r.Vol, v, delta, tau)
	phiStar := math.Hypot(phi, sigma)
	newPhi := 1 / math.Sqrt(1/(phiStar*phiStar)+1/v)
	newMu := mu + newPhi*newPhi*weightedScore
	return Rating{
		R:   InitialR + glickoScale*newMu,
		RD:  math.Min(MaxRD, glickoScale*newPhi),
		Vol: sigma,
	}
}

// Decay applies periods empty rating periods, preserving rating and volatility.
func Decay(r Rating, periods int) Rating {
	if periods <= 0 {
		return r
	}
	phi := r.RD / glickoScale
	r.RD = math.Min(MaxRD, glickoScale*math.Sqrt(phi*phi+float64(periods)*r.Vol*r.Vol))
	return r
}

// Conservative is the two-RD lower bound of a rating.
func Conservative(r Rating) float64 { return r.R - 2*r.RD }

func glickoG(phi float64) float64 {
	return 1 / math.Sqrt(1+3*phi*phi/(math.Pi*math.Pi))
}

func glickoExpected(muA, muB, phiB float64) float64 {
	return 1 / (1 + math.Exp(-glickoG(phiB)*(muA-muB)))
}

// glickoVolatility uses the Illinois root-finding iteration from Glickman's
// Glicko-2 worked example.
func glickoVolatility(phi, sigma, v, delta, tau float64) float64 {
	a := math.Log(sigma * sigma)
	root := func(x float64) float64 {
		ex := math.Exp(x)
		denom := phi*phi + v + ex
		return ex*(delta*delta-phi*phi-v-ex)/(2*denom*denom) - (x-a)/(tau*tau)
	}
	A := a
	var B float64
	if delta*delta > phi*phi+v {
		B = math.Log(delta*delta - phi*phi - v)
	} else {
		for k := 1; ; k++ {
			B = a - float64(k)*tau
			if root(B) >= 0 {
				break
			}
		}
	}
	fA, fB := root(A), root(B)
	for math.Abs(B-A) > glickoEpsilon {
		C := A + (A-B)*fA/(fB-fA)
		fC := root(C)
		if fC*fB <= 0 {
			A, fA = B, fB
		} else {
			fA /= 2
		}
		B, fB = C, fC
	}
	return math.Exp(A / 2)
}
