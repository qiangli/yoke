package weave

import (
	"fmt"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/spf13/cobra"
)

// Shared by reactive takeover and predictive quota supervision. The linked
// sprint lease supplies the current recipient; never infer it from the worker.
func weaveSupervisionAlarm(cmd *cobra.Command, dir string, run *weaveItem, kind, message string) error {
	board, s, err := weaveTakeoverSprint(dir, run)
	if err != nil {
		return err
	}
	message = takeoverRedact(message)
	if err := withWeaveQueueLock(board, func(q *weaveQueue) error {
		current := findWeaveStory(q, s.ID)
		if current == nil || current.Lease == nil {
			return fmt.Errorf("linked sprint lease disappeared")
		}
		s = current
		weaveStoryAppend(current, "supervisor", kind, message)
		return nil
	}); err != nil {
		return err
	}
	if len(message) > 1000 {
		message = message[:1000]
	}
	return weaveSupervisionSend(s.Lease.Holder, message)
}

var weaveSupervisionSend = func(to, body string) error {
	_, err := bus.Send(bus.SendRequest{From: "supervisor", To: to, Topic: "sprint", Body: body})
	return err
}

func weaveTakeoverAlarm(cmd *cobra.Command, dir string, it *weaveItem, e weaveTakeoverEvent) error {
	return weaveSupervisionAlarm(cmd, dir, it, "takeover-alarm", fmt.Sprintf("run #%d story %s: %s died; cause=%s; evidence=%s; takeover=%s; mode=%s; status=%s", it.ID, e.Story, e.From, e.Cause, e.Evidence, e.To, e.Mode, e.Status))
}
