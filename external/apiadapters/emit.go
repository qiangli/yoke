package apiadapters

import (
	"encoding/json"
	"io"

	"github.com/qiangli/yoke/external/gws"
)

// EmitPage writes typed items as one loom-v2 page envelope through the
// shared gws emitter (the single `schema_version: loom-v2`
// discriminator): status ok when NextToken is empty, partial with the
// resume token otherwise. Total counts this page; provider listings here
// expose no grand total, and this function never invents one.
func EmitPage(w io.Writer, command string, items []any, nextToken string) int {
	raw := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			return gws.NewError(gws.CodeProviderFailure, "apiadapters: unencodable item", nil).EmitError(w, command)
		}
		raw = append(raw, b)
	}
	return gws.EmitPage(w, command, gws.Page{Items: raw, NextToken: nextToken, Complete: nextToken == "", Total: len(raw)})
}
