package room

import (
	"context"
	"errors"
	"testing"
)

func TestReadDeltaAppendRestartRotation(t *testing.T) {
	isolate(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	disableAutoRotation(t)
	ctx := context.Background()
	if err := Emit(Event{Type: EventNotify, Body: "first"}); err != nil {
		t.Fatal(err)
	}
	a, cursor, err := ReadDelta(ctx, "")
	if err != nil || len(a) != 1 || a[0].Seq != 1 {
		t.Fatalf("first: %+v %v", a, err)
	}
	firstCursor := cursor
	if a, _, err = ReadDelta(ctx, cursor); err != nil || len(a) != 0 {
		t.Fatalf("idle: %+v %v", a, err)
	}
	if err := Emit(Event{Type: EventNotify, Body: "second"}); err != nil {
		t.Fatal(err)
	}
	a, cursor, err = ReadDelta(ctx, cursor)
	if err != nil || len(a) != 1 || a[0].Body != "second" || a[0].Seq != 2 {
		t.Fatalf("append: %+v %v", a, err)
	}
	if err := rewriteTimeline([]Event{{Type: EventNotify, Body: "second"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveWatermark(1); err != nil {
		t.Fatal(err)
	}
	if err := Emit(Event{Type: EventNotify, Body: "third"}); err != nil {
		t.Fatal(err)
	}
	a, next, err := ReadDelta(ctx, cursor)
	if err != nil || len(a) != 1 || a[0].Body != "third" || a[0].Seq != 3 {
		t.Fatalf("rotation: %+v %v", a, err)
	}
	if a, _, err = ReadDelta(ctx, next); err != nil || len(a) != 0 {
		t.Fatalf("restart: %+v %v", a, err)
	}
	if err := rewriteTimeline([]Event{{Type: EventNotify, Body: "third"}}); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveWatermark(2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadDelta(ctx, firstCursor); !errors.Is(err, ErrDeltaExpired) {
		t.Fatalf("expired: %v", err)
	}
}
