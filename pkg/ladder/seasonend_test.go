package ladder

import "testing"

func seasonendTestLines() Lines {
	return Lines{
		L3Code:   1500,
		L4Code:   1600,
		L4Manage: 1500,
		L5Code:   1700,
		L5Manage: 1600,
		L5Judge:  1600,
	}
}

// seasonendTestCerts returns valid certificates for every gate up to band.
func seasonendTestCerts(band int, version string, season int) []Certificate {
	var kinds []CertKind
	if band >= 1 {
		kinds = append(kinds, CertL1)
	}
	if band >= 2 {
		kinds = append(kinds, CertL2)
	}
	if band >= 3 {
		kinds = append(kinds, CertL3, CertSteer)
	}
	if band >= 4 {
		kinds = append(kinds, CertManager, CertReview)
	}
	if band >= 5 {
		kinds = append(kinds, CertJudge, CertL5)
	}
	certs := make([]Certificate, 0, len(kinds))
	for _, k := range kinds {
		certs = append(certs, Certificate{Kind: k, ModelVersion: version, Season: season})
	}
	return certs
}

// seasonendTestProfile builds a profile whose gates hold through band, with
// every duty standing carrying the given event count.
func seasonendTestProfile(band, events int, version string, season int) Profile {
	standings := map[Duty]DutyStanding{
		DutyCode:   {R: 1900, RD: 50, Events: events},
		DutyManage: {R: 1800, RD: 50, Events: events},
		DutyJudge:  {R: 1800, RD: 50, Events: events},
	}
	return Profile{
		Standings:    standings,
		Certs:        seasonendTestCerts(band, version, season),
		ModelVersion: version,
	}
}

func seasonendTestOptions(season int) SeasonEndOptions {
	return SeasonEndOptions{
		Season:                     season,
		Lines:                      seasonendTestLines(),
		ProvisionalDeadlineSeasons: 2,
		PegExpirySeasons:           2,
	}
}

func TestSeasonEndPromotionNeedsEstablished(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 2, ModelVersion: "v1"},
	}
	// Gates for band 3 hold, but code has only 3 rated events.
	profiles := map[string]Profile{
		"agent-a": seasonendTestProfile(3, 3, "v1", 7),
	}
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(7))
	if len(changes) != 0 {
		t.Fatalf("unestablished standings must not promote, got changes %+v", changes)
	}
	if next["agent-a"].Band != 2 {
		t.Fatalf("band = %d, want 2", next["agent-a"].Band)
	}

	// Same profile with established standings promotes.
	profiles["agent-a"] = seasonendTestProfile(3, EstablishedEvents, "v1", 7)
	next, changes = SeasonEnd(states, profiles, seasonendTestOptions(7))
	if len(changes) != 1 || changes[0].From != 2 || changes[0].To != 3 || changes[0].Reason != "promotion" {
		t.Fatalf("changes = %+v, want one 2->3 promotion", changes)
	}
	if next["agent-a"].Band != 3 {
		t.Fatalf("band = %d, want 3", next["agent-a"].Band)
	}
}

func TestSeasonEndMultiBandPromotion(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 1, ModelVersion: "v1"},
	}
	profiles := map[string]Profile{
		"agent-a": seasonendTestProfile(4, 20, "v1", 3),
	}
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(3))
	if len(changes) != 1 || changes[0].From != 1 || changes[0].To != 4 || changes[0].Reason != "promotion" {
		t.Fatalf("changes = %+v, want one 1->4 promotion", changes)
	}
	if next["agent-a"].Band != 4 {
		t.Fatalf("band = %d, want 4", next["agent-a"].Band)
	}
}

func TestSeasonEndDirectDemotion(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 5, ModelVersion: "v1"},
	}
	// Only G1+G2 hold now: certificates above l2 are gone.
	profiles := map[string]Profile{
		"agent-a": seasonendTestProfile(2, 20, "v1", 9),
	}
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(9))
	if len(changes) != 1 || changes[0].From != 5 || changes[0].To != 2 || changes[0].Reason != "demotion" {
		t.Fatalf("changes = %+v, want one direct 5->2 demotion", changes)
	}
	got := next["agent-a"]
	if got.Band != 2 {
		t.Fatalf("band = %d, want 2", got.Band)
	}
	if got.Demoted[5] != 9 {
		t.Fatalf("Demoted[5] = %d, want 9", got.Demoted[5])
	}
	// Input state must not be mutated.
	if states["agent-a"].Demoted != nil {
		t.Fatalf("input state mutated: %+v", states["agent-a"])
	}
}

func TestSeasonEndHysteresisBlocksNextSeasonOnly(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 3, Demoted: map[int]int{4: 10}, ModelVersion: "v1"},
	}
	profiles := map[string]Profile{
		"agent-a": seasonendTestProfile(4, 20, "v1", 11),
	}
	// Season 11 = the season right after the demotion: re-promotion to 4 blocked.
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(11))
	if len(changes) != 0 {
		t.Fatalf("hysteresis season must not re-promote, got %+v", changes)
	}
	if next["agent-a"].Band != 3 {
		t.Fatalf("band = %d, want 3", next["agent-a"].Band)
	}

	// Season 12: allowed again.
	profiles["agent-a"] = seasonendTestProfile(4, 20, "v1", 12)
	next, changes = SeasonEnd(states, profiles, seasonendTestOptions(12))
	if len(changes) != 1 || changes[0].From != 3 || changes[0].To != 4 || changes[0].Reason != "promotion" {
		t.Fatalf("changes = %+v, want one 3->4 promotion", changes)
	}
	if next["agent-a"].Band != 4 {
		t.Fatalf("band = %d, want 4", next["agent-a"].Band)
	}
}

func TestSeasonEndHysteresisCapsPartialPromotion(t *testing.T) {
	// Demoted from 5 last season, currently 3, now derives 5: capped at 4.
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 3, Demoted: map[int]int{5: 20}, ModelVersion: "v1"},
	}
	profiles := map[string]Profile{
		"agent-a": seasonendTestProfile(5, 20, "v1", 21),
	}
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(21))
	if len(changes) != 1 || changes[0].From != 3 || changes[0].To != 4 || changes[0].Reason != "hysteresis" {
		t.Fatalf("changes = %+v, want one 3->4 hysteresis-capped promotion", changes)
	}
	if next["agent-a"].Band != 4 {
		t.Fatalf("band = %d, want 4", next["agent-a"].Band)
	}
}

func TestSeasonEndProvisionalKeptThenExpired(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 5, ProvisionalSince: 1, ModelVersion: "v1"},
	}
	// Real capability is band 2; the L5 seat is provisional.
	profile := seasonendTestProfile(2, 20, "v1", 2)
	profile.Provisional = 5
	profiles := map[string]Profile{"agent-a": profile}

	// Season 2: within the 2-season deadline, seat kept.
	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(2))
	if len(changes) != 0 {
		t.Fatalf("provisional within deadline must not change, got %+v", changes)
	}
	if next["agent-a"].Band != 5 {
		t.Fatalf("band = %d, want 5", next["agent-a"].Band)
	}

	// Season 3 (= ProvisionalSince + 2): expired, drops to derived band.
	profile = seasonendTestProfile(2, 20, "v1", 3)
	profile.Provisional = 5
	profiles["agent-a"] = profile
	next, changes = SeasonEnd(next, profiles, seasonendTestOptions(3))
	if len(changes) != 1 || changes[0].From != 5 || changes[0].To != 2 || changes[0].Reason != "provisional expired" {
		t.Fatalf("changes = %+v, want one 5->2 provisional expiry", changes)
	}
	got := next["agent-a"]
	if got.Band != 2 {
		t.Fatalf("band = %d, want 2", got.Band)
	}
	if got.ProvisionalSince != 0 {
		t.Fatalf("ProvisionalSince = %d, want cleared", got.ProvisionalSince)
	}
}

func TestSeasonEndProvisionalConfirmedEarly(t *testing.T) {
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 5, ProvisionalSince: 1, ModelVersion: "v1"},
	}
	profile := seasonendTestProfile(5, 20, "v1", 2)
	profile.Provisional = 5
	profiles := map[string]Profile{"agent-a": profile}

	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(2))
	if len(changes) != 1 || changes[0].From != 5 || changes[0].To != 5 || changes[0].Reason != "confirmed" {
		t.Fatalf("changes = %+v, want one 5->5 confirmation", changes)
	}
	got := next["agent-a"]
	if got.Band != 5 {
		t.Fatalf("band = %d, want 5", got.Band)
	}
	if got.ProvisionalSince != 0 {
		t.Fatalf("ProvisionalSince = %d, want cleared", got.ProvisionalSince)
	}
}

func TestVersionBump(t *testing.T) {
	prev := map[Duty]DutyStanding{
		DutyCode:   {R: 1900, RD: 60, Events: 40},
		DutyManage: {R: 1700, RD: 90, Events: 12},
	}
	certs := seasonendTestCerts(4, "v1", 5)

	standings, voided := VersionBump(prev, certs)
	if len(voided) != 0 {
		t.Fatalf("certificates must be voided, got %d", len(voided))
	}
	for duty, want := range prev {
		got, ok := standings[duty]
		if !ok {
			t.Fatalf("missing %s standing after bump", duty)
		}
		if got.R != want.R {
			t.Fatalf("%s R = %g, want %g (kept)", duty, got.R, want.R)
		}
		if got.RD != InitialRD {
			t.Fatalf("%s RD = %g, want %g (reset)", duty, got.RD, InitialRD)
		}
		if got.Events != want.Events {
			t.Fatalf("%s Events = %d, want %d (kept)", duty, got.Events, want.Events)
		}
	}
	// Input must not be mutated.
	if prev[DutyCode].RD != 60 {
		t.Fatalf("input standings mutated: %+v", prev[DutyCode])
	}
}

func TestSeasonEndVersionGraceOneSeason(t *testing.T) {
	// A bumped version keeps the predecessor's band for one season even
	// though its certificates are voided (derived band 0).
	states := map[string]SeasonState{
		"agent-a": {Agent: "agent-a", Band: 4, PredecessorOf: "agent-a-prev", ModelVersion: "v2"},
	}
	profile := seasonendTestProfile(4, 20, "v2", 6)
	profile.Certs = nil // voided by VersionBump
	profiles := map[string]Profile{"agent-a": profile}

	next, changes := SeasonEnd(states, profiles, seasonendTestOptions(6))
	if len(changes) != 1 || changes[0].From != 4 || changes[0].To != 4 || changes[0].Reason != "version grace" {
		t.Fatalf("changes = %+v, want one 4->4 version grace", changes)
	}
	got := next["agent-a"]
	if got.Band != 4 {
		t.Fatalf("band = %d, want 4 (grace)", got.Band)
	}
	if got.PredecessorOf != "" {
		t.Fatalf("PredecessorOf = %q, want cleared after the grace season", got.PredecessorOf)
	}

	// Next season, still not re-certified: normal direct demotion applies.
	next, changes = SeasonEnd(next, profiles, seasonendTestOptions(7))
	if len(changes) != 1 || changes[0].From != 4 || changes[0].To != 0 || changes[0].Reason != "demotion" {
		t.Fatalf("changes = %+v, want one 4->0 demotion after grace", changes)
	}
	if next["agent-a"].Band != 0 {
		t.Fatalf("band = %d, want 0", next["agent-a"].Band)
	}
	if next["agent-a"].Demoted[4] != 7 {
		t.Fatalf("Demoted[4] = %d, want 7", next["agent-a"].Demoted[4])
	}
}
