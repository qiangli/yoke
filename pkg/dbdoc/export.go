package dbdoc

import "encoding/json"

// CompileFilter is the document filter compiler for another category that
// stores a JSON object in a column named doc (db vec metadata): the same
// grammar, bound parameters and errors as db doc find.
func CompileFilter(filter, params json.RawMessage) (where string, args []any, err error) {
	return compileFilter(filter, params)
}

// CanonicalObject strictly decodes a JSON object (duplicate keys refused,
// numbers verbatim, at most MaxDocBytes) and returns the text db doc stores.
func CanonicalObject(what string, raw json.RawMessage) ([]byte, error) {
	m, err := object(what, raw)
	if err != nil {
		return nil, err
	}
	return encodeJSON(m)
}
