package guard

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	pw := []string{"hunter2-initial", "1234", "ab"}
	in := map[string]any{
		"value":  "hunter2-initial",
		"nested": []any{"x", "login hunter2-initial ok", map[string]any{"pin": float64(1234)}},
		"short":  "ab",
		"embed":  "cab",
		"name":   "ada",
		"n":      float64(7),
		"ok":     true,
	}
	got := RedactSecrets(in, pw).(map[string]any)
	want := map[string]any{
		"value":  PasswordMark,
		"nested": []any{"x", "login " + PasswordMark + " ok", map[string]any{"pin": PasswordMark}},
		"short":  PasswordMark,
		"embed":  "cab",
		"name":   "ada",
		"n":      float64(7),
		"ok":     true,
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		t.Fatalf("got %s", g)
	}
	if RedactSecrets("hunter2-initial", nil) != "hunter2-initial" {
		t.Fatal("no secrets must be a no-op")
	}
}
