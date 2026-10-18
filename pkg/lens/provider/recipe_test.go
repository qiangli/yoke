package provider

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

func TestRecipeAdmissionAndJSONL(t *testing.T) {
	r := NewRegistry()
	if err := r.RegisterRecipe("s3", Recipe{}); err == nil {
		t.Fatal("s3 admitted")
	}
	recipe := Recipe{ListJSONL: func(context.Context, ref.Locator) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"schema_version":"bashy-node-v1","ref":"oci:image","path":"image","name":"image","kind":"record","type":"oci/image","attrs":{}}` + "\n")), nil
	}}
	if err := r.RegisterRecipe("oci", recipe); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Parse("oci:images"); err != nil {
		t.Fatal(err)
	}
	p, ok := r.Lookup("oci")
	if !ok {
		t.Fatal("missing oci")
	}
	rows, err := p.List(context.Background(), ref.Locator{Scheme: "oci", Target: "images"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
