package guard

import (
	"fmt"
	"strconv"
	"strings"
)

// PasswordMark replaces a raw password-input value in a read result.
const PasswordMark = "[guarded:password]"

// RedactSecrets returns v with every secret removed from its leaves: a
// string equal to a secret (or, for secrets of 4+ characters, containing it)
// and a number whose text equals one. Structure, keys and every other value
// are preserved. This is result sanitization for what the page's own
// password inputs hold, not a sandbox for hostile scripts.
func RedactSecrets(v any, secrets []string) any {
	if len(secrets) == 0 {
		return v
	}
	return redactAny(v, secrets)
}

func redactAny(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		for _, s := range secrets {
			if x == s {
				return PasswordMark
			}
			if len(s) >= 4 && strings.Contains(x, s) {
				x = strings.ReplaceAll(x, s, PasswordMark)
			}
		}
		return x
	case float64, float32, int, int64, int32, uint, uint64, uint32:
		if t := numText(x); t != "" {
			for _, s := range secrets {
				if t == s {
					return PasswordMark
				}
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactAny(x[i], secrets)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = redactAny(e, secrets)
		}
		return x
	}
	return v
}

func numText(n any) string {
	switch x := n.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	}
	return fmt.Sprint(n)
}
