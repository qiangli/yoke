package fleet

import "sort"

// Derived names are computed here, once, after the rings have merged — and
// never written back to disk.
//
// They have to be computed at the catalog level rather than on the entry
// because they are a question about the whole set, not about one row: "is
// this the newest opus?" cannot be answered by looking at one opus. And they
// have to stay out of the store because they float: baking `opus -> opus5`
// into a file freezes the pointer the next release is supposed to move.
//
// One rule governs all of it: A DERIVED NAME NEVER SHADOWS A DECLARED ONE.
// If anything in the catalog already answers to a name, the derivation
// yields. That is what keeps CheckAliases clean and keeps whois from ever
// having to guess between something an operator wrote down and something we
// made up.

// decorateModels attaches each family's floating alias to the newest member
// of that family.
func decorateModels(models []Model) {
	taken := map[string]bool{}
	for _, m := range models {
		for _, n := range m.Names() {
			taken[n] = true
		}
	}

	// The newest member of each family, by version. Ties break on name so
	// two identically-versioned entries still pick the same winner twice.
	best := map[string]int{}
	for i, m := range models {
		if m.Family == "" {
			continue
		}
		j, seen := best[m.Family]
		if !seen {
			best[m.Family] = i
			continue
		}
		switch CompareVersions(m.Version, models[j].Version) {
		case 1:
			best[m.Family] = i
		case 0:
			if m.Name < models[j].Name {
				best[m.Family] = i
			}
		}
	}

	for _, fam := range sortedKeys(best) {
		if taken[fam] {
			continue
		}
		i := best[fam]
		models[i].Derived = append(models[i].Derived, fam)
		taken[fam] = true
	}
}

// decorateAgents gives every agent the two names it did not declare: the
// floating family alias (`claude-opus`, which follows opus from 4.8 to 4.9
// on its own) and a human nickname.
//
// Family aliases are assigned before nicknames so a drawn nickname can never
// take a slot a family alias needs; within each pass agents are walked in
// canonical-name order, so the collision probing is reproducible.
func decorateAgents(agents []Agent, models []Model) {
	family := map[string]string{} // canonical model name -> its family, if it is the newest
	decorateModels(models)
	canon := map[string]string{} // any name a model answers to -> its canonical name
	for _, m := range models {
		for _, n := range m.Names() {
			canon[n] = m.Name
		}
		for _, d := range m.Derived {
			if d == m.Family {
				family[m.Name] = m.Family
			}
		}
	}

	// An agent may DECLARE its model by any name — `model: fable` is a
	// perfectly natural thing to write. But MatrixKey is the identity that
	// gets recorded, and a key built from a floating alias floats: an
	// attestation saying `claude:fable` silently means a different model
	// after the next release. Canonicalize on the way in, so the identity is
	// version-explicit no matter how the binding was spelled.
	for i := range agents {
		if c, ok := canon[agents[i].Model]; ok {
			agents[i].Model = c
		}
	}

	taken := map[string]bool{}
	for _, a := range agents {
		for _, n := range a.Names() {
			taken[n] = true
		}
	}

	order := make([]int, len(agents))
	for i := range agents {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return agents[order[i]].Name < agents[order[j]].Name })

	for _, i := range order {
		fam, ok := family[agents[i].Model]
		if !ok {
			continue // this agent binds an older version; the alias belongs to the newest
		}
		// Cascade agents carry their BASE's model, while clones carry their
		// parent's binding. Neither is the durable plain binding denoted by a
		// floating family alias: `ycode-glm` must not resolve to an escalation
		// cascade or a temporary evaluation clone. A plain agent later in name
		// order still takes it. Check Ephemeral separately so a malformed or
		// partially-written clone cannot steal the alias either.
		if agents[i].IsCascade() || agents[i].ClonedFrom != "" || agents[i].Ephemeral {
			continue
		}
		alias := agents[i].Tool + "-" + fam
		if taken[alias] {
			continue
		}
		agents[i].Derived = append(agents[i].Derived, alias)
		taken[alias] = true
	}

	for _, i := range order {
		if agents[i].Nick != "" {
			continue
		}
		if n := assignNickname(agents[i].MatrixKey(), taken); n != "" {
			agents[i].AutoNick = n
			taken[n] = true
		}
	}
}
