package guard

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/qiangli/yoke/pkg/browser/wire"
	"github.com/qiangli/yoke/pkg/sensitive"
)

// Mask replaces every vault value in a result's text with
// [guarded:<kind>]: a vault secret a page echoes back is not handed to the
// agent. Other page text is read unmasked. Image bytes are not inspected — screenshots are a recorded gap.
func Mask(ctx context.Context, r *wire.Result) *wire.Result {
	if r == nil {
		return nil
	}
	vm := loadVault(ctx)
	r.Title = maskString(r.Title, vm)
	r.URL = maskString(r.URL, vm)
	r.Content = maskString(r.Content, vm)
	r.Elements = maskString(r.Elements, vm)
	r.Error = maskString(r.Error, vm)
	r.Data = maskData(r.Data, vm)
	return r
}

// MaskText masks one string (exposed for callers that print page text
// outside a wire.Result).
func MaskText(ctx context.Context, s string) string { return maskString(s, loadVault(ctx)) }

// spansOf finds guarded spans. Reads (strict=false) mask only vault values:
// page text is handed over unmasked. The ask record (strict=true) also masks
// validator-shaped values.
func spansOf(s string, vm *vaultMatcher, strict bool) []sensitive.Span {
	if s == "" {
		return nil
	}
	var sp []sensitive.Span
	if strict {
		sp = sensitive.Scan(s)
	}
	sp = append(sp, vm.spans(s)...)
	return sensitive.ResolveOverlaps(sp)
}

func maskString(s string, vm *vaultMatcher) string { return maskWith(s, vm, false) }

func maskWith(s string, vm *vaultMatcher, strict bool) string {
	spans := spansOf(s, vm, strict)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(s[last:sp.Start])
		b.WriteString("[guarded:" + sp.Kind + "]")
		last = sp.End
	}
	b.WriteString(s[last:])
	return b.String()
}

// maskData masks a Data payload. Structured JSON is masked leaf by leaf so
// the payload stays valid JSON (a number is never rewritten into a bare
// token); anything else is masked as text.
func maskData(s string, vm *vaultMatcher) string {
	if len(spansOf(s, vm, false)) == 0 {
		return s
	}
	t := strings.TrimSpace(s)
	if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		var v any
		dec := json.NewDecoder(strings.NewReader(t))
		dec.UseNumber()
		if dec.Decode(&v) == nil {
			if b, err := json.Marshal(maskJSON(v, vm)); err == nil {
				return string(b)
			}
		}
	}
	return maskString(s, vm)
}

func maskJSON(v any, vm *vaultMatcher) any {
	switch x := v.(type) {
	case string:
		return maskString(x, vm)
	case json.Number:
		if len(spansOf(x.String(), vm, false)) > 0 {
			return "[guarded]"
		}
		return x
	case []any:
		for i := range x {
			x[i] = maskJSON(x[i], vm)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = maskJSON(e, vm)
		}
		return x
	}
	return v
}
