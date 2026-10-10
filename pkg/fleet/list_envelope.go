package fleet

import "io"

// ListSchemaVersion versions the envelope every registry kind's `list --json`
// emits. `show --json` stays the bare record: it is what `add` reads back.
const ListSchemaVersion = "bashy-registry-list-v1"

type listEnvelope[T any] struct {
	SchemaVersion string `json:"schema_version"`
	Kind          string `json:"kind"`
	View          string `json:"view"`
	Items         []T    `json:"items"`
}

// WriteListJSON is the one writer of a kind's `list --json`. A nil slice is
// an empty list, never null.
func WriteListJSON[T any](w io.Writer, kind, view string, items []T) error {
	if items == nil {
		items = []T{}
	}
	return writeJSON(w, listEnvelope[T]{SchemaVersion: ListSchemaVersion, Kind: kind, View: view, Items: items})
}

// BasicView names the view of a list that only has --retired and, for
// commands, --all.
func BasicView(retired, all bool) string {
	switch {
	case retired:
		return "retired"
	case all:
		return "all"
	}
	return "default"
}
