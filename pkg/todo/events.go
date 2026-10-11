package todo

import (
	"fmt"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/room"
)

// recordStatusEvent puts a status change on the host event log (bashy events).
// Like notifyAssignee it is called from the CLI layer only, so SetStatus stays
// hermetic, and it is best effort: the change already committed.
func recordStatusEvent(it *issue.Issue) {
	actor, _ := bus.ResolveAuthoredActor("")
	room.Record(room.EventTodo, actor, "todo:"+shortID(it.ID), it.Status, fmt.Sprintf("%s: %s", it.Status, it.Title))
}
