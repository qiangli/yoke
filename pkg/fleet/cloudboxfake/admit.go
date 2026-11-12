package cloudboxfake

import "net/http"

// AdmitRule is the audience rule shared with host and suite grants.
type AdmitRule struct {
	Circles []string `json:"circles"`
	Groups  []string `json:"groups"`
	Emails  []string `json:"emails"`
}

func (r *AdmitRule) normalize() {
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

func (f *Fake) satelliteAdmit(w http.ResponseWriter, r *http.Request, id string) *apiError {
	scope := ScopeSatellitesRead
	if r.Method == http.MethodPut {
		scope = ScopeSatellitesWrite
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		return errf(404, "not_found", "no route")
	}
	t, e := f.auth(r, scope)
	if e != nil {
		return e
	}
	sat := f.sats[id]
	if sat == nil || sat.sat.OwnerEmail != t.email {
		return errf(403, "not_owner", "satellite is not owned by caller")
	}
	if r.Method == http.MethodPut {
		var body struct {
			Rules []AdmitRule `json:"rules"`
		}
		if e := decode(r, &body); e != nil {
			return e
		}
		if body.Rules == nil {
			return errf(400, "invalid_rules", "rules array required")
		}
		for i := range body.Rules {
			rule := &body.Rules[i]
			for _, c := range rule.Circles {
				if validSlug(c, "invalid_circle") != nil {
					return errf(400, "invalid_circle", "invalid circle %q", c)
				}
			}
			for _, g := range rule.Groups {
				if validSlug(g, "invalid_group") != nil {
					return errf(400, "invalid_group", "invalid group %q", g)
				}
			}
			rule.normalize()
		}
		f.admitRules[id] = body.Rules
	}
	rules := append([]AdmitRule{}, f.admitRules[id]...)
	for i := range rules {
		rules[i].normalize()
	}
	writeJSON(w, 200, map[string]any{"satellite": id, "rules": rules})
	return nil
}
