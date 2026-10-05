package python

import (
	"testing"
)

func TestUvEnvAddsMuslOnlyWhenNeeded(t *testing.T) {
	env := uvEnv([]string{"A=1"})
	if needsMusl() {
		if len(env) != 3 || env[1] != "UV_LIBC=musl" {
			t.Fatalf("uvEnv on a musl host = %q", env)
		}
	} else if len(env) != 1 {
		t.Fatalf("uvEnv on a glibc/non-linux host = %q", env)
	}
}
