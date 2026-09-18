package fleet

import (
	"strconv"
	"strings"
)

// MaxBand is the top capability band. Four is a deliberate ceiling, not a
// placeholder: bands exist to make a coarse routing decision cheap ("who is
// worth seating?"), and a ladder fine enough to argue about would just be
// the quality score with extra steps.
const MaxBand = 4

// CompareVersions orders two model versions, returning -1, 0, or +1.
//
// Versions are dotted sequences whose segments are compared NUMERICALLY
// where both sides are numbers: 4.10 is newer than 4.8, which plain string
// ordering gets backwards — and getting it backwards would silently point
// the floating family alias at a stale model, which is the one failure this
// whole mechanism exists to prevent.
//
// A shorter version is older than a longer one that agrees on every shared
// segment (4 < 4.1), so a bare `5` loses to `5.1`. Non-numeric segments fall
// back to string order, which is arbitrary but stable — enough to keep the
// comparator total so the highest-version pick is never ambiguous.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareSegment(as[i], bs[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func compareSegment(a, b string) int {
	an, aerr := strconv.Atoi(a)
	bn, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	}
	// A plain number outranks a tagged segment, so `4` beats `4rc` and a
	// release candidate never steals the family alias from the release.
	switch {
	case aerr == nil && berr != nil:
		return 1
	case aerr != nil && berr == nil:
		return -1
	}
	return strings.Compare(a, b)
}
