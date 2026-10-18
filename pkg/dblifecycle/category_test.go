package dblifecycle

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/policy/advice"
)

// Declare falls back to Spec without SpecInput; Admit refuses a declared
// net.* effect only under an effect cap that lacks net.
func TestDeclareAndAdmitNetEffect(t *testing.T) {
	base := func(mode dbstore.Mode, a Access) Operation {
		return Operation{Name: "test.op", MutationClass: "read", Effects: []string{"db.read"}}
	}
	plain := CategoryOp{Spec: base}
	if s := plain.Declare(json.RawMessage(`{"net":true}`), dbstore.Memory, AccessRead); !slices.Equal(s.Effects, []string{"db.read"}) {
		t.Fatalf("fallback declaration %v", s.Effects)
	}
	aware := CategoryOp{Spec: base, SpecInput: func(raw json.RawMessage, mode dbstore.Mode, a Access) Operation {
		s := base(mode, a)
		var in struct{ Net bool }
		if json.Unmarshal(raw, &in) == nil && in.Net {
			s.Effects = append(s.Effects, "net.egress")
		}
		return s
	}}
	local := aware.Declare(json.RawMessage(`{}`), dbstore.Memory, AccessRead)
	remote := aware.Declare(json.RawMessage(`{"net":true}`), dbstore.Memory, AccessRead)
	if slices.Contains(local.Effects, "net.egress") || !slices.Contains(remote.Effects, "net.egress") {
		t.Fatalf("input-aware declaration: %v / %v", local.Effects, remote.Effects)
	}
	noNet, _ := advice.ParseCap("read,write")
	withNet, _ := advice.ParseCap("read,net")
	for _, tc := range []struct {
		ctx  context.Context
		spec Operation
		code string
	}{
		{context.Background(), remote, ""},
		{advice.WithCap(context.Background(), noNet), local, ""},
		{advice.WithCap(context.Background(), noNet), remote, "unsupported"},
		{advice.WithCap(context.Background(), withNet), remote, ""},
	} {
		if got := dbstore.Code(Admit(tc.ctx, tc.spec)); got != tc.code {
			t.Fatalf("Admit %v: %q want %q", tc.spec.Effects, got, tc.code)
		}
	}
}
