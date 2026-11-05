package issue

import (
	"reflect"
	"testing"
)

func TestShapeMetadataPreservesExistingFrontmatter(t *testing.T) {
	input := []byte("---\nid: abc123\nkind: task\ntitle: Existing\nstatus: doing\nlabels: [existing]\nsprint: 564\nsprint_id: stable-sprint\ncustom_owner:\n  department: examples\n---\n\nOriginal body.\n")
	item, err := Parse(input)
	if err != nil {
		t.Fatal(err)
	}
	extra := item.Extra
	item.Origin = "shape"
	item.Shape = []ShapeChange{{App: "hello-world", Candidate: "1234567890abcdef", Actor: "agent", RepresentedUser: "person"}}
	AddLabels(item, []string{"shaping"})
	data, err := item.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "doing" || got.Body != "Original body." || got.Sprint != 564 || got.SprintID != "stable-sprint" || !reflect.DeepEqual(got.Extra, extra) {
		t.Fatalf("metadata changed: %#v", got)
	}
	if !reflect.DeepEqual(got.Labels, []string{"existing", "shaping"}) || got.Origin != "shape" || !reflect.DeepEqual(got.Shape, item.Shape) {
		t.Fatalf("shape metadata did not roundtrip: %#v", got)
	}
}
