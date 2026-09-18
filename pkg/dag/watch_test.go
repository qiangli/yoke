// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"reflect"
	"testing"
)

func TestChangedTargets(t *testing.T) {
	prev := map[string]string{"a": "1", "b": "2", "gone": "x"}
	cur := map[string]string{"a": "1", "b": "3", "new": "y"}
	got := changedTargets(prev, cur)
	want := []string{"b", "gone", "new"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedTargets = %v, want %v", got, want)
	}
}

func TestAffectedTargetsIncludesDependents(t *testing.T) {
	md := "## Tasks\n\n" +
		"### src\n" + block("bash", "echo src") +
		"### build\nRequires: src\n" + block("bash", "echo build") +
		"### test\nRequires: build\n" + block("bash", "echo test") +
		"### other\n" + block("bash", "echo other")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatal(err)
	}
	got := affectedTargets(g, []string{"build"})
	want := []string{"build", "test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("affectedTargets = %v, want %v", got, want)
	}
}
