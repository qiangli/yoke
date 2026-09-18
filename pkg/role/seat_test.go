package role

import (
	"testing"
	"time"
)

var seatNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// THE TRI-STATE IS THE POINT, and the reason this is not a bool. Two of the
// three seats this replaces could not say "I don't know", so a missing or
// impossible heartbeat read as HELD — the one answer that makes a successor
// wait for somebody who is never coming back.
func TestSeat_Liveness(t *testing.T) {
	ttl := 30 * time.Minute
	cases := []struct {
		name string
		seat Seat
		want Liveness
	}{
		{"nobody holds it",
			Seat{TTL: ttl}, LivenessVacant},
		{"held, heartbeat fresh",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-time.Hour), HeartbeatAt: seatNow.Add(-time.Minute), TTL: ttl},
			LivenessLive},
		{"held, heartbeat older than the TTL",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-2 * time.Hour), HeartbeatAt: seatNow.Add(-time.Hour), TTL: ttl},
			LivenessLapsed},
		{"held, but no heartbeat was ever recorded",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-time.Hour), TTL: ttl},
			LivenessUnknown},
		// A future heartbeat would never lapse, so a dead holder would stay
		// "live" forever. That is worse than any wrong verdict this could give.
		{"heartbeat from the future",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-time.Hour), HeartbeatAt: seatNow.Add(time.Hour), TTL: ttl},
			LivenessUnknown},
		// Left by whoever held the seat last; it says nothing about the current
		// occupant.
		{"heartbeat predates the tenure",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-time.Minute), HeartbeatAt: seatNow.Add(-time.Hour), TTL: ttl},
			LivenessUnknown},
		// Ordinary clock and filesystem-granularity drift must not be read as a
		// broken record.
		{"heartbeat a moment ahead is ordinary skew",
			Seat{Holder: "a", AcquiredAt: seatNow.Add(-time.Hour), HeartbeatAt: seatNow.Add(time.Second), TTL: ttl},
			LivenessLive},
	}
	for _, c := range cases {
		if got := c.seat.Live(seatNow); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// UNKNOWN IS NOT TAKEABLE. A lapsed seat is evidence the holder stopped; an
// unknown one is the absence of evidence either way, and seizing on it is how a
// successor takes a seat somebody is still holding.
func TestSeat_UnknownIsNotTakeable(t *testing.T) {
	if LivenessUnknown.Takeable() {
		t.Error("unknown must not be takeable — go and look, do not seize")
	}
	for _, l := range []Liveness{LivenessVacant, LivenessLapsed} {
		if !l.Takeable() {
			t.Errorf("%v should be takeable without the incumbent's cooperation", l)
		}
	}
	if LivenessLive.Takeable() {
		t.Error("a live seat must never be takeable")
	}
}

// A seat with no TTL never lapses on time alone — the caller has not said how
// long a heartbeat stays believable, and inventing a default would silently
// evict holders under a rule nobody chose.
func TestSeat_NoTTLDoesNotLapse(t *testing.T) {
	s := Seat{Holder: "a", AcquiredAt: seatNow.Add(-99 * time.Hour), HeartbeatAt: seatNow.Add(-99 * time.Hour)}
	if got := s.Live(seatNow); got != LivenessLive {
		t.Errorf("got %v, want live — no TTL means no expiry rule was given", got)
	}
}
