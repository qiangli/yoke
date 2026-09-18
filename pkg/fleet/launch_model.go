package fleet

import (
	"sort"
	"strings"
	"unicode"
)

// ResolveLaunchModel maps the provider display string persisted in a launch
// record back to the catalog's canonical model name. Matching is deliberately
// conservative: normalized canonical names must occur as a substring and the
// longest match wins. Unknown values stay unknown (band zero); this function
// never guesses a neighboring model.
func ResolveLaunchModel(tool, launchModelDisplay string) (band int, canonical string) {
	return resolveLaunchModel(New(), tool, launchModelDisplay)
}

// ResolveLaunchModel is the catalog-bound form of ResolveLaunchModel, for
// callers that already hold a (possibly pinned) catalog.
func (c *Catalog) ResolveLaunchModel(tool, launchModelDisplay string) (band int, canonical string) {
	return resolveLaunchModel(c, tool, launchModelDisplay)
}

func resolveLaunchModel(cat *Catalog, tool, display string) (int, string) {
	want := normalizeModelName(display)
	if want == "" {
		return 0, display
	}
	models, _ := cat.Models()
	sort.SliceStable(models, func(i, j int) bool {
		return len(normalizeModelName(models[i].Name)) > len(normalizeModelName(models[j].Name))
	})
	for _, m := range models {
		needles := []string{m.Name}
		// A launch record may contain the tool-specific provider spelling. It
		// still resolves to m.Name; no alias is ever returned as canonical.
		if id := m.TargetFor(tool); id != "" {
			needles = append(needles, id)
		}
		if m.Display != "" {
			needles = append(needles, m.Display)
		}
		sort.SliceStable(needles, func(i, j int) bool {
			return len(normalizeModelName(needles[i])) > len(normalizeModelName(needles[j]))
		})
		for _, needle := range needles {
			n := normalizeModelName(needle)
			if n != "" && strings.Contains(want, n) {
				return m.Band, m.Name
			}
		}
	}
	return 0, display
}

func normalizeModelName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
