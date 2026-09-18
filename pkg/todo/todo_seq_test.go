// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package todo

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
)

func TestAddAssignsIncrementingSeq(t *testing.T) {
	st := &issue.Store{Root: t.TempDir(), Sub: RepoSub}
	a, err := Add(st, "first", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Add(st, "second", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("running numbers not incrementing: first=%d second=%d", a.Seq, b.Seq)
	}
}

func TestResolveRefByNumberAndPrefix(t *testing.T) {
	st := &issue.Store{Root: t.TempDir(), Sub: RepoSub}
	// Explicit letter-containing id so the prefix is never parsed as a number.
	it := &issue.Issue{ID: "abc123def456", Kind: issue.KindTask, Seq: 5, Title: "x", Status: StatusTodo, Created: time.Now().UTC()}
	if _, err := st.Save(it); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveRef(st, "5"); err != nil || got.ID != "abc123def456" {
		t.Fatalf("resolve by number: got %v err %v", got, err)
	}
	if got, err := ResolveRef(st, "abc123"); err != nil || got.Seq != 5 {
		t.Fatalf("resolve by id prefix: got %v err %v", got, err)
	}
	if _, err := ResolveRef(st, "99"); err == nil {
		t.Fatal("expected an error resolving an unknown number")
	}
}

func TestEnsureSeqBackfillsInCreationOrder(t *testing.T) {
	st := &issue.Store{Root: t.TempDir(), Sub: RepoSub}
	older := &issue.Issue{ID: "aaaa11112222", Kind: issue.KindTask, Title: "older", Status: StatusTodo, Created: time.Now().UTC().Add(-time.Hour)}
	newer := &issue.Issue{ID: "bbbb33334444", Kind: issue.KindTask, Title: "newer", Status: StatusTodo, Created: time.Now().UTC()}
	if _, err := st.Save(older); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(newer); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSeq(st); err != nil {
		t.Fatal(err)
	}
	ro, _ := st.Resolve("aaaa1111")
	rn, _ := st.Resolve("bbbb3333")
	if ro.Seq != 1 || rn.Seq != 2 {
		t.Fatalf("backfill not in creation order: older=%d newer=%d", ro.Seq, rn.Seq)
	}
	// Idempotent: a second pass changes nothing, and a NEW item continues the count.
	if err := EnsureSeq(st); err != nil {
		t.Fatal(err)
	}
	next, err := Add(st, "third", "", "", nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq != 3 {
		t.Fatalf("new item should continue the sequence, got %d", next.Seq)
	}
}
