package bus

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestPostMessageOnceChild(t *testing.T) {
	if os.Getenv("BUS_ONCE_CHILD") != "1" {
		return
	}
	if _, err := PostMessageOnce(context.Background(), "shared-key", Post{From: "observer", To: "manager", Body: "resource condition"}); err != nil {
		t.Fatal(err)
	}
}
func TestPostMessageOnceConcurrentProcesses(t *testing.T) {
	boardInTempHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var children []*exec.Cmd
	for i := 0; i < 3; i++ {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPostMessageOnceChild$")
		c.Env = append(os.Environ(), "BUS_ONCE_CHILD=1")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, c)
	}
	for _, c := range children {
		if err := c.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	posts, err := Posts()
	if err != nil || len(posts) != 1 {
		t.Fatalf("duplicate publication: %+v %v", posts, err)
	}
}
func TestPostMessageOnceRecoversAppendBeforeReceipt(t *testing.T) {
	boardInTempHome(t)
	post := Post{From: "observer", To: "manager", Body: "memory high"}
	postOnceAfterAppend = func() error { return errors.New("injected crash after append") }
	t.Cleanup(func() { postOnceAfterAppend = nil })
	if _, err := PostMessageOnce(context.Background(), "condition-1", post); err == nil {
		t.Fatal("fault ignored")
	}
	postOnceAfterAppend = nil
	seq, err := PostMessageOnce(context.Background(), "condition-1", post)
	if err != nil || seq != 1 {
		t.Fatalf("recovery=%d %v", seq, err)
	}
	posts, err := Posts()
	if err != nil || len(posts) != 1 {
		t.Fatalf("recovery duplicated: %+v %v", posts, err)
	}
	post.Body = "different content"
	if _, err := PostMessageOnce(context.Background(), "condition-1", post); err == nil {
		t.Fatal("key accepted conflicting content")
	}
}
