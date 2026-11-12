package fleet

// grantRule is the common audience shape used by host grants, suite grants,
// and satellite admission. Empty slices are always emitted as JSON arrays.
type grantRule struct {
	Circles []string `json:"circles"`
	Groups  []string `json:"groups"`
	Emails  []string `json:"emails"`
}

func newGrantRule(circles, groups, emails []string) grantRule {
	r := grantRule{Circles: append([]string{}, circles...), Groups: append([]string{}, groups...), Emails: append([]string{}, emails...)}
	return r
}

func (r *grantRule) normalize() {
	if r.Circles == nil {
		r.Circles = []string{}
	}
	if r.Groups == nil {
		r.Groups = []string{}
	}
	if r.Emails == nil {
		r.Emails = []string{}
	}
}
