package kb

import "strings"

// NearDuplicate finds the existing live page a new (title, description) most
// likely duplicates: same slug, or a high token overlap on the title (or on
// title+description combined). Blind appends are the documented death
// spiral of agent memory — every add reconciles first, and a hit means the
// caller should UPDATE or SUPERSEDE that page instead (or --force past it).
func NearDuplicate(pages []*Page, title, description string) *Page {
	slug := Slugify(title)
	newTitle := tokenSet(title)
	newBoth := tokenSet(title + " " + description)
	var best *Page
	bestScore := 0.0
	for _, p := range pages {
		if p.Status == StatusSuperseded {
			continue
		}
		if p.Slug == slug {
			return p
		}
		tj := jaccard(newTitle, tokenSet(p.Title))
		bj := jaccard(newBoth, tokenSet(p.Title+" "+p.Description))
		score := 0.0
		if tj >= 0.6 {
			score = tj + 1 // title match dominates
		} else if bj >= 0.5 {
			score = bj
		}
		if score > bestScore {
			bestScore = score
			best = p
		}
	}
	return best
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for f := range strings.FieldsSeq(strings.ToLower(s)) {
		f = strings.Trim(f, ".,;:!?()[]{}'\"`")
		if len(f) >= 2 {
			out[f] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for t := range a {
		if b[t] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}
