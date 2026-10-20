// Package hostsrc wires the generic watch engine (pkg/watch) to the journals
// the host ALREADY keeps: the message board (pkg/bus) and the sprint thread
// projection on the room timeline (pkg/room).
//
// It is a separate package for two reasons. The engine stays dependency-light,
// so pkg/resources and anything else can embed it without pulling the board in;
// and the direction of knowledge stays one-way — a source knows about a store,
// a store never knows it is being watched. A command becomes watchable by
// emitting to the journal it already owns, which both of these do, so nothing
// here required a change to either store.
//
// Both sources READ. Nothing in this package writes to a journal except the
// inbox sink, which sends through the board's own documented send path and is
// injectable precisely so that no test ever sends real mail.
package hostsrc

import (
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/watch"
)

// Board is the slice of the message board a watcher needs: the public log, the
// per-reader cursor that makes a READ receipt possible, and the send path the
// inbox sink posts through.
//
// It is an interface for one reason that is worth the indirection: a test must
// be able to exercise reply tracking and the inbox sink against a fake. Mail is
// append-only and public — a test that posted to the real board could not take
// it back, and that has happened on this host before.
type Board interface {
	Posts() ([]bus.Post, error)
	SeenSeq(reader string) int64
	Post(bus.Post) error
}

// LiveBoard is the host's real board.
type LiveBoard struct{}

// Posts implements Board.
func (LiveBoard) Posts() ([]bus.Post, error) { return bus.Posts() }

// SeenSeq implements Board.
func (LiveBoard) SeenSeq(reader string) int64 { return bus.SeenSeq(reader) }

// Post implements Board.
func (LiveBoard) Post(p bus.Post) error { return bus.PostMessage(p) }

// Timeline reads the host event log, newest-last.
type Timeline func(n int) ([]room.Event, error)

// DefaultBoard and DefaultTimeline are the stores the registered source
// factories read. They are package VARIABLES, following pkg/bus' own watch
// seam, so an integration test can point the sprint preset at a fixture board
// and a fixture timeline without a parallel construction path — the thing under
// test is then the preset as shipped.
var (
	DefaultBoard    Board    = LiveBoard{}
	DefaultTimeline Timeline = room.Timeline
)

func init() {
	watch.RegisterSource("mb", func(spec watch.SourceSpec) (watch.Source, error) {
		src := NewMBSource(DefaultBoard)
		src.Backfill = spec.Backfill.D()
		src.Recipient = spec.Target
		return src, nil
	})
	watch.RegisterSource("sprint-thread", func(spec watch.SourceSpec) (watch.Source, error) {
		src := NewSprintSource(DefaultTimeline)
		src.Backfill = spec.Backfill.D()
		src.Sprint = spec.Target
		return src, nil
	})
	watch.RegisterJoin("mb-reply", MBReplyLevels)
}
