package fleet

import "hash/fnv"

// Agents need a name a human can say. `opencode-kimi-k2.7-code` is an
// address, not a name — you cannot ask someone to "get Kimi-k2.7-code's
// read on this" without reading it off a screen first.
//
// So every agent gets one, assigned rather than configured. The assignment
// is DETERMINISTIC in the binding: the same tool:model draws the same name
// on every host, forever, with no state file to sync and nothing to migrate.
// A random-then-persisted name would give two machines two different names
// for the same agent, which defeats the point of having a name at all.
//
// An operator who wants a different one says so — `agents set X --nick Bond`
// — and the explicit name wins.

// nicknames is the draw pool: short, pronounceable, and culturally spread,
// with no two entries close enough to mishear for one another.
var nicknames = []string{
	"Ada", "Alba", "Alma", "Amara", "Anders", "Anouk", "Arlo", "Asa",
	"Astrid", "Aurelio", "Bahar", "Basil", "Beatrix", "Bela", "Bruno",
	"Caleb", "Calla", "Camilo", "Cassian", "Cato", "Cedric", "Cleo",
	"Cosmo", "Dagny", "Dalia", "Damaris", "Dashiell", "Delia", "Dmitri",
	"Dorian", "Edda", "Eero", "Elif", "Elio", "Eloise", "Emeric", "Esme",
	"Ewan", "Fabian", "Faris", "Felix", "Fenna", "Flora", "Franka",
	"Gabor", "Galen", "Gemma", "Gideon", "Gita", "Greta", "Gustav",
	"Hana", "Harlan", "Hedda", "Hugo", "Idris", "Ilya", "Imani", "Ines",
	"Ingrid", "Iris", "Isolde", "Ivar", "Jasper", "Javier", "Jelena",
	"Johnny", "Jonas", "Juno", "Kaia", "Kamal", "Karim", "Kasper",
	"Keiko", "Kenji", "Kira", "Klara", "Lars", "Leilani", "Lennox",
	"Leonor", "Linnea", "Lorcan", "Lucia", "Ludo", "Magnus", "Maia",
	"Malik", "Marek", "Margit", "Mateo", "Maud", "Merida", "Milena",
	"Mira", "Nadia", "Nash", "Nell", "Niamh", "Nico", "Nils", "Noor",
	"Nova", "Oleg", "Olive", "Omar", "Onyx", "Orla", "Oscar", "Otto",
	"Paloma", "Pascal", "Petra", "Piper", "Quill", "Quinn", "Rafferty",
	"Raisa", "Ramona", "Reza", "Rhea", "Rilke", "Roan", "Romy", "Rosalind",
	"Rufus", "Sable", "Sasha", "Saoirse", "Selim", "Senna", "Silas",
	"Sirin", "Solvei", "Soren", "Stellan", "Sunniva", "Tadeo", "Talia",
	"Tarek", "Thea", "Theo", "Tobias", "Ulla", "Umberto", "Ursula",
	"Vala", "Vesna", "Viggo", "Vera", "Wilhelmina", "Wren", "Xavier",
	"Yara", "Yusuf", "Zadie", "Zephyr", "Zora",
}

// assignNickname draws a name for a binding, skipping any the catalog has
// already claimed.
//
// The draw is the binding's hash, so it is stable across hosts and across
// runs; on a collision it probes forward through the pool, which keeps the
// result unique without making it random. `taken` is every name the catalog
// already answers to, so an assigned nickname can never shadow a declared
// one — see decorate, which walks agents in sorted order so the probe
// sequence is itself deterministic.
func assignNickname(binding string, taken map[string]bool) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(binding))
	start := int(h.Sum32() % uint32(len(nicknames)))
	for i := 0; i < len(nicknames); i++ {
		n := nicknames[(start+i)%len(nicknames)]
		if !taken[n] {
			return n
		}
	}
	// The pool is exhausted — more agents than names. Better to have no
	// nickname than a duplicate one: a name that means two agents is worse
	// than no name at all, because whois would have to guess.
	return ""
}
