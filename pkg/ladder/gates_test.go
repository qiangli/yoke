package ladder

import (
	"strings"
	"testing"
)

func TestDutyStanding(t *testing.T) {
	s := DutyStanding{R: 1500, RD: 50, Events: 7}
	if got, want := s.Lower(), 1400.0; got != want {
		t.Fatalf("Lower() = %v, want %v", got, want)
	}
	if s.Established() {
		t.Fatalf("Established() = true for 7 events, want false (min %d)", EstablishedEvents)
	}

	s.Events = 8
	if !s.Established() {
		t.Fatalf("Established() = false for 8 events, want true")
	}

	s.Events = 15
	if !s.Established() {
		t.Fatalf("Established() = false for 15 events, want true")
	}
}

func TestCertValid(t *testing.T) {
	c := Certificate{
		Kind:         CertL1,
		ModelVersion: "model-v1",
		Season:       1,
	}

	// Valid in same season
	if !CertValid(c, "model-v1", 1) {
		t.Errorf("CertValid season 1 = false, want true")
	}

	// Valid within expiry window (< 3 seasons elapsed)
	if !CertValid(c, "model-v1", 2) { // 2 - 1 = 1
		t.Errorf("CertValid season 2 = false, want true")
	}
	if !CertValid(c, "model-v1", 3) { // 3 - 1 = 2
		t.Errorf("CertValid season 3 = false, want true")
	}

	// Expired after 3 seasons
	if CertValid(c, "model-v1", 4) { // 4 - 1 = 3
		t.Errorf("CertValid season 4 = true, want false (expired after %d seasons)", CertExpirySeasons)
	}
	if CertValid(c, "model-v1", 5) {
		t.Errorf("CertValid season 5 = true, want false")
	}

	// Invalid if season is before issuance
	if CertValid(c, "model-v1", 0) {
		t.Errorf("CertValid season 0 = true, want false")
	}

	// Immediately invalid on model version bump
	if CertValid(c, "model-v2", 1) {
		t.Errorf("CertValid mismatched version = true, want false")
	}
}

func TestMedianOfAndMedianCode(t *testing.T) {
	// Empty slice
	if got := medianOf(nil); got != 0 {
		t.Errorf("medianOf(nil) = %v, want 0", got)
	}
	if got := medianOf([]float64{}); got != 0 {
		t.Errorf("medianOf([]) = %v, want 0", got)
	}

	// Odd count
	if got, want := medianOf([]float64{1500}), 1500.0; got != want {
		t.Errorf("medianOf([1500]) = %v, want %v", got, want)
	}
	if got, want := medianOf([]float64{1600, 1400, 1500}), 1500.0; got != want {
		t.Errorf("medianOf([1600, 1400, 1500]) = %v, want %v", got, want)
	}

	// Even count (mean of middle two)
	if got, want := medianOf([]float64{1400, 1600}), 1500.0; got != want {
		t.Errorf("medianOf([1400, 1600]) = %v, want %v", got, want)
	}
	if got, want := medianOf([]float64{1700, 1400, 1600, 1500}), 1550.0; got != want {
		t.Errorf("medianOf([1700, 1400, 1600, 1500]) = %v, want %v", got, want)
	}

	// MedianCode: only established standings count
	standings := []DutyStanding{
		{R: 1300, RD: 30, Events: 8},  // established
		{R: 1900, RD: 100, Events: 3}, // unestablished, should be ignored
		{R: 1500, RD: 40, Events: 10}, // established
		{R: 1400, RD: 50, Events: 9},  // established
		{R: 2000, RD: 200, Events: 0}, // unestablished
	}
	// Established ratings are [1300, 1400, 1500] -> median is 1400
	if got, want := MedianCode(standings), 1400.0; got != want {
		t.Errorf("MedianCode() = %v, want %v", got, want)
	}

	// No established standings -> 0
	unestablished := []DutyStanding{
		{R: 1500, RD: 50, Events: 7},
		{R: 1600, RD: 40, Events: 1},
	}
	if got := MedianCode(unestablished); got != 0 {
		t.Errorf("MedianCode(unestablished) = %v, want 0", got)
	}
}

func TestDominanceOK(t *testing.T) {
	// Reviewer Lower() >= Author R
	// Reviewer R=1600, RD=50 -> Lower = 1500
	reviewer := DutyStanding{R: 1600, RD: 50, Events: 10}

	// Author R=1500 -> 1500 >= 1500 (equal is OK)
	authorEqual := DutyStanding{R: 1500, RD: 30, Events: 10}
	if !DominanceOK(reviewer, authorEqual) {
		t.Errorf("DominanceOK equal = false, want true")
	}

	// Author R=1480 -> 1500 >= 1480 (reviewer strictly better)
	authorLower := DutyStanding{R: 1480, RD: 30, Events: 10}
	if !DominanceOK(reviewer, authorLower) {
		t.Errorf("DominanceOK author lower = false, want true")
	}

	// Author R=1520 -> 1500 < 1520 (nobody reviews uphill)
	authorHigher := DutyStanding{R: 1520, RD: 30, Events: 10}
	if DominanceOK(reviewer, authorHigher) {
		t.Errorf("DominanceOK author higher = true, want false (nobody reviews uphill)")
	}
}

func defaultLines() Lines {
	return Lines{
		L3Code:   1400,
		L4Code:   1500,
		L4Manage: 1450,
		L5Code:   1600,
		L5Manage: 1550,
		L5Judge:  1500,
	}
}

func TestDeriveBand_TableDriven(t *testing.T) {
	lines := defaultLines()
	mv := "model-v1"
	season := 1

	tests := []struct {
		name         string
		profile      Profile
		wantBand     int
		wantGateMiss int
		wantReason   string
	}{
		{
			name: "no certs at all: band 0 blocked by G1",
			profile: Profile{
				ModelVersion: mv,
			},
			wantBand:     0,
			wantGateMiss: 1,
			wantReason:   "no valid l1 certificate",
		},
		{
			name: "only L1 cert: band 1 blocked by G2",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
				},
			},
			wantBand:     1,
			wantGateMiss: 2,
			wantReason:   "no valid l2 certificate",
		},
		{
			name: "L1 and L2 certs: band 2 blocked by G3 missing certs and standing",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
				},
			},
			wantBand:     2,
			wantGateMiss: 3,
			wantReason:   "no valid l3 certificate",
		},
		{
			name: "L1..L3 + steer certs, but code.Lower < L3Code",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode: {R: 1450, RD: 50, Events: 10}, // Lower() = 1350 < 1400
				},
			},
			wantBand:     2,
			wantGateMiss: 3,
			wantReason:   "conservative code 1350 < L3 line 1400",
		},
		{
			name: "L3 achieved: G1..G3 hold, blocked by G4 missing manager cert",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode: {R: 1550, RD: 50, Events: 10}, // Lower() = 1450 >= 1400
				},
			},
			wantBand:     3,
			wantGateMiss: 4,
			wantReason:   "no valid manager certificate",
		},
		{
			name: "G4 certs present, but code.Lower < L4Code",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
					{Kind: CertManager, ModelVersion: mv, Season: season},
					{Kind: CertReview, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode:   {R: 1550, RD: 50, Events: 10}, // Lower() = 1450 < 1500
					DutyManage: {R: 1600, RD: 50, Events: 10}, // Lower() = 1500 >= 1450
				},
			},
			wantBand:     3,
			wantGateMiss: 4,
			wantReason:   "conservative code 1450 < L4 line 1500",
		},
		{
			name: "G4 certs present, code >= L4Code, but manage.Lower < L4Manage",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
					{Kind: CertManager, ModelVersion: mv, Season: season},
					{Kind: CertReview, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode:   {R: 1620, RD: 50, Events: 10}, // Lower() = 1520 >= 1500
					DutyManage: {R: 1480, RD: 50, Events: 10}, // Lower() = 1380 < 1450
				},
			},
			wantBand:     3,
			wantGateMiss: 4,
			wantReason:   "conservative manage 1380 < L4 line 1450",
		},
		{
			name: "L4 achieved: G1..G4 hold, blocked by G5",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
					{Kind: CertManager, ModelVersion: mv, Season: season},
					{Kind: CertReview, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode:   {R: 1620, RD: 50, Events: 10}, // Lower() = 1520 >= 1500
					DutyManage: {R: 1560, RD: 50, Events: 10}, // Lower() = 1460 >= 1450
				},
			},
			wantBand:     4,
			wantGateMiss: 5,
			wantReason:   "no valid judge certificate",
		},
		{
			name: "L5 achieved: G1..G5 hold, no misses",
			profile: Profile{
				ModelVersion: mv,
				Certs: []Certificate{
					{Kind: CertL1, ModelVersion: mv, Season: season},
					{Kind: CertL2, ModelVersion: mv, Season: season},
					{Kind: CertL3, ModelVersion: mv, Season: season},
					{Kind: CertSteer, ModelVersion: mv, Season: season},
					{Kind: CertManager, ModelVersion: mv, Season: season},
					{Kind: CertReview, ModelVersion: mv, Season: season},
					{Kind: CertJudge, ModelVersion: mv, Season: season},
					{Kind: CertL5, ModelVersion: mv, Season: season},
				},
				Standings: map[Duty]DutyStanding{
					DutyCode:   {R: 1720, RD: 50, Events: 10}, // Lower() = 1620 >= 1600
					DutyManage: {R: 1680, RD: 50, Events: 10}, // Lower() = 1580 >= 1550
					DutyJudge:  {R: 1620, RD: 50, Events: 10}, // Lower() = 1520 >= 1500
				},
			},
			wantBand:     5,
			wantGateMiss: 0,
			wantReason:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			band, missing := DeriveBand(tt.profile, lines, season)
			if band != tt.wantBand {
				t.Errorf("DeriveBand() band = %d, want %d", band, tt.wantBand)
			}
			if tt.wantGateMiss == 0 {
				if len(missing) != 0 {
					t.Errorf("DeriveBand() missing = %+v, want none", missing)
				}
			} else {
				if len(missing) == 0 {
					t.Fatalf("DeriveBand() missing empty, want gate %d miss", tt.wantGateMiss)
				}
				if missing[0].Gate != tt.wantGateMiss {
					t.Errorf("DeriveBand() missing gate = %d, want %d", missing[0].Gate, tt.wantGateMiss)
				}
				if tt.wantReason != "" {
					found := false
					for _, m := range missing {
						if strings.Contains(m.Reason, tt.wantReason) {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("missing reasons %+v do not contain %q", missing, tt.wantReason)
					}
				}
			}
		})
	}
}

func TestDeriveBand_CumulativeRule(t *testing.T) {
	// A strong judge who codes below the L4 line cannot be L4 or L5.
	// Bands are strictly cumulative: Ln requires every duty of every band below it.
	lines := defaultLines()
	mv := "model-v1"
	season := 1

	p := Profile{
		ModelVersion: mv,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: season},
			{Kind: CertL2, ModelVersion: mv, Season: season},
			{Kind: CertL3, ModelVersion: mv, Season: season},
			{Kind: CertSteer, ModelVersion: mv, Season: season},
			{Kind: CertManager, ModelVersion: mv, Season: season},
			{Kind: CertReview, ModelVersion: mv, Season: season},
			{Kind: CertJudge, ModelVersion: mv, Season: season},
			{Kind: CertL5, ModelVersion: mv, Season: season},
		},
		Standings: map[Duty]DutyStanding{
			// Passes L3Code (1400) with Lower=1450, but fails L4Code (1500)
			DutyCode:   {R: 1550, RD: 50, Events: 12},
			DutyManage: {R: 1700, RD: 40, Events: 12}, // Lower=1620, easily passes L4/L5 manage
			DutyJudge:  {R: 1700, RD: 40, Events: 12}, // Lower=1620, easily passes L5 judge
		},
	}

	band, missing := DeriveBand(p, lines, season)
	if band != 3 {
		t.Fatalf("DeriveBand() = %d, want 3 (cumulative rule caps at weakest duty)", band)
	}
	if len(missing) == 0 || missing[0].Gate != 4 {
		t.Fatalf("missing = %+v, want gate 4 blocking", missing)
	}
	if !strings.Contains(missing[0].Reason, "conservative code 1450 < L4 line 1500") {
		t.Errorf("missing reason = %q, want 'conservative code 1450 < L4 line 1500'", missing[0].Reason)
	}
}

func TestDeriveBand_LapseAtLowerDuty(t *testing.T) {
	// An L5 whose code rating lapses (e.g. RD widens from inactivity) immediately drops band.
	lines := defaultLines()
	mv := "model-v1"
	season := 1

	p := Profile{
		ModelVersion: mv,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: season},
			{Kind: CertL2, ModelVersion: mv, Season: season},
			{Kind: CertL3, ModelVersion: mv, Season: season},
			{Kind: CertSteer, ModelVersion: mv, Season: season},
			{Kind: CertManager, ModelVersion: mv, Season: season},
			{Kind: CertReview, ModelVersion: mv, Season: season},
			{Kind: CertJudge, ModelVersion: mv, Season: season},
			{Kind: CertL5, ModelVersion: mv, Season: season},
		},
		Standings: map[Duty]DutyStanding{
			// Previously Lower=1620, now RD widened to 150 -> Lower = 1720 - 300 = 1420
			// Passes L3Code (1400), fails L4Code (1500)
			DutyCode:   {R: 1720, RD: 150, Events: 10},
			DutyManage: {R: 1680, RD: 50, Events: 10},
			DutyJudge:  {R: 1620, RD: 50, Events: 10},
		},
	}

	band, missing := DeriveBand(p, lines, season)
	if band != 3 {
		t.Fatalf("DeriveBand() = %d, want 3 after code lapse", band)
	}
	if len(missing) == 0 || missing[0].Gate != 4 {
		t.Fatalf("missing = %+v, want gate 4 blocking", missing)
	}
}

func TestDeriveBand_ZeroLineFailsClosed(t *testing.T) {
	// A zero line means "line not yet fitted" and fails closed.
	// Fleet evidence invariant: no success by absence.
	mv := "model-v1"
	season := 1

	baseProfile := Profile{
		ModelVersion: mv,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: season},
			{Kind: CertL2, ModelVersion: mv, Season: season},
			{Kind: CertL3, ModelVersion: mv, Season: season},
			{Kind: CertSteer, ModelVersion: mv, Season: season},
			{Kind: CertManager, ModelVersion: mv, Season: season},
			{Kind: CertReview, ModelVersion: mv, Season: season},
			{Kind: CertJudge, ModelVersion: mv, Season: season},
			{Kind: CertL5, ModelVersion: mv, Season: season},
		},
		Standings: map[Duty]DutyStanding{
			DutyCode:   {R: 1900, RD: 20, Events: 20}, // Lower = 1860
			DutyManage: {R: 1900, RD: 20, Events: 20}, // Lower = 1860
			DutyJudge:  {R: 1900, RD: 20, Events: 20}, // Lower = 1860
		},
	}

	// 1. L3Code = 0 -> fails closed at Gate 3
	linesL3Zero := defaultLines()
	linesL3Zero.L3Code = 0
	band, missing := DeriveBand(baseProfile, linesL3Zero, season)
	if band != 2 {
		t.Errorf("band = %d, want 2 when L3Code=0 (fails closed)", band)
	}
	if len(missing) == 0 || missing[0].Gate != 3 || !strings.Contains(missing[0].Reason, "not yet fitted") {
		t.Errorf("missing = %+v, want gate 3 line not yet fitted", missing)
	}

	// 2. L4Code = 0 -> fails closed at Gate 4
	linesL4CodeZero := defaultLines()
	linesL4CodeZero.L4Code = 0
	band, missing = DeriveBand(baseProfile, linesL4CodeZero, season)
	if band != 3 {
		t.Errorf("band = %d, want 3 when L4Code=0 (fails closed)", band)
	}
	if len(missing) == 0 || missing[0].Gate != 4 || !strings.Contains(missing[0].Reason, "not yet fitted") {
		t.Errorf("missing = %+v, want gate 4 line not yet fitted", missing)
	}

	// 3. L4Manage = 0 -> fails closed at Gate 4
	linesL4ManageZero := defaultLines()
	linesL4ManageZero.L4Manage = 0
	band, missing = DeriveBand(baseProfile, linesL4ManageZero, season)
	if band != 3 {
		t.Errorf("band = %d, want 3 when L4Manage=0 (fails closed)", band)
	}
	if len(missing) == 0 || missing[0].Gate != 4 || !strings.Contains(missing[0].Reason, "not yet fitted") {
		t.Errorf("missing = %+v, want gate 4 line not yet fitted", missing)
	}

	// 4. L5Judge = 0 -> fails closed at Gate 5
	linesL5JudgeZero := defaultLines()
	linesL5JudgeZero.L5Judge = 0
	band, missing = DeriveBand(baseProfile, linesL5JudgeZero, season)
	if band != 4 {
		t.Errorf("band = %d, want 4 when L5Judge=0 (fails closed)", band)
	}
	if len(missing) == 0 || missing[0].Gate != 5 || !strings.Contains(missing[0].Reason, "not yet fitted") {
		t.Errorf("missing = %+v, want gate 5 line not yet fitted", missing)
	}
}

func TestDeriveBand_CertExpiry(t *testing.T) {
	lines := defaultLines()
	mv := "model-v1"

	p := Profile{
		ModelVersion: mv,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: 1},
			{Kind: CertL2, ModelVersion: mv, Season: 1},
			{Kind: CertL3, ModelVersion: mv, Season: 1},
			{Kind: CertSteer, ModelVersion: mv, Season: 1},
		},
		Standings: map[Duty]DutyStanding{
			DutyCode: {R: 1550, RD: 50, Events: 10},
		},
	}

	// Season 3: elapsed = 3 - 1 = 2 < 3 seasons -> still valid (band 3)
	band3, _ := DeriveBand(p, lines, 3)
	if band3 != 3 {
		t.Errorf("DeriveBand() season 3 = %d, want 3", band3)
	}

	// Season 4: elapsed = 4 - 1 = 3 >= 3 seasons -> expired, drops to band 0
	band4, missing4 := DeriveBand(p, lines, 4)
	if band4 != 0 {
		t.Errorf("DeriveBand() season 4 = %d, want 0 (expired certs)", band4)
	}
	if len(missing4) == 0 || missing4[0].Gate != 1 {
		t.Errorf("missing = %+v, want Gate 1 miss", missing4)
	}

	// Re-certification in season 4 for L1 only: now passes G1, but G2 is still expired
	p.Certs = append(p.Certs, Certificate{Kind: CertL1, ModelVersion: mv, Season: 4})
	band4Renewed, missing4Renewed := DeriveBand(p, lines, 4)
	if band4Renewed != 1 {
		t.Errorf("DeriveBand() with renewed L1 = %d, want 1", band4Renewed)
	}
	if len(missing4Renewed) == 0 || missing4Renewed[0].Gate != 2 {
		t.Errorf("missing = %+v, want Gate 2 miss", missing4Renewed)
	}
}

func TestDeriveBand_VersionBump(t *testing.T) {
	lines := defaultLines()

	p := Profile{
		ModelVersion: "model-v1",
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: "model-v1", Season: 1},
			{Kind: CertL2, ModelVersion: "model-v1", Season: 1},
		},
	}

	// With matching version: band 2
	band, _ := DeriveBand(p, lines, 1)
	if band != 2 {
		t.Fatalf("DeriveBand() = %d, want 2", band)
	}

	// Model version bumped to model-v2: certs voided immediately
	p.ModelVersion = "model-v2"
	bandBumped, missingBumped := DeriveBand(p, lines, 1)
	if bandBumped != 0 {
		t.Fatalf("DeriveBand() after version bump = %d, want 0", bandBumped)
	}
	if len(missingBumped) == 0 || missingBumped[0].Gate != 1 {
		t.Errorf("missing = %+v, want Gate 1 miss", missingBumped)
	}
}

func TestDeriveBand_ProvisionalSeat(t *testing.T) {
	lines := defaultLines()
	mv := "model-v1"
	season := 1

	// Case 1: Agent with 0 certs (derived = 0), but Provisional = 5 (e.g. bootstrap judge)
	pBootstrap := Profile{
		ModelVersion: mv,
		Provisional:  5,
	}

	band, missing := DeriveBand(pBootstrap, lines, season)
	if band != 5 {
		t.Errorf("DeriveBand() band = %d, want 5 (provisional seat)", band)
	}
	// Missing reports real gaps for the derived band (derived = 0 -> Gate 1 gaps)
	if len(missing) == 0 || missing[0].Gate != 1 {
		t.Errorf("missing = %+v, want real gaps for derived band 0 (Gate 1)", missing)
	}

	// Case 2: Agent derived band is 3, provisional is 4
	pCandidate := Profile{
		ModelVersion: mv,
		Provisional:  4,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: season},
			{Kind: CertL2, ModelVersion: mv, Season: season},
			{Kind: CertL3, ModelVersion: mv, Season: season},
			{Kind: CertSteer, ModelVersion: mv, Season: season},
		},
		Standings: map[Duty]DutyStanding{
			DutyCode: {R: 1550, RD: 50, Events: 10}, // Lower = 1450 >= 1400
		},
	}

	bandCand, missingCand := DeriveBand(pCandidate, lines, season)
	if bandCand != 4 {
		t.Errorf("DeriveBand() band = %d, want 4 (provisional seat)", bandCand)
	}
	// Real gaps are for derived band 3 -> Gate 4 gaps
	if len(missingCand) == 0 || missingCand[0].Gate != 4 {
		t.Errorf("missing = %+v, want real gaps for derived band 3 (Gate 4)", missingCand)
	}

	// Case 3: Provisional is lower than derived band -> derived wins
	pCandidate.Provisional = 2
	bandLower, missingLower := DeriveBand(pCandidate, lines, season)
	if bandLower != 3 {
		t.Errorf("DeriveBand() band = %d, want 3 (derived exceeds provisional)", bandLower)
	}
	if len(missingLower) == 0 || missingLower[0].Gate != 4 {
		t.Errorf("missing = %+v, want Gate 4 gaps", missingLower)
	}
}

func TestDeriveBand_MissingStanding(t *testing.T) {
	// A missing duty standing fails any gate that needs it.
	lines := defaultLines()
	mv := "model-v1"
	season := 1

	// Gate 3 needs DutyCode
	p := Profile{
		ModelVersion: mv,
		Certs: []Certificate{
			{Kind: CertL1, ModelVersion: mv, Season: season},
			{Kind: CertL2, ModelVersion: mv, Season: season},
			{Kind: CertL3, ModelVersion: mv, Season: season},
			{Kind: CertSteer, ModelVersion: mv, Season: season},
		},
		Standings: nil, // missing DutyCode standing
	}

	band, missing := DeriveBand(p, lines, season)
	if band != 2 {
		t.Errorf("band = %d, want 2 when code standing missing", band)
	}
	found := false
	for _, m := range missing {
		if m.Gate == 3 && strings.Contains(m.Reason, "missing code standing") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("missing = %+v, want 'missing code standing'", missing)
	}
}
