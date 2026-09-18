package meet

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs wires Meet's durable room references into the shared ref registry.
func RegisterRefs(g *ref.Registry) {
	g.Register(ref.Meet, ref.ResolverFunc(resolveRef))
}

func resolveRef(id string) (ref.Node, error) {
	fullID, err := resolveMeeting(id)
	if err != nil {
		if notFoundMeetingError(err) {
			return ref.Node{}, fmt.Errorf("%w: meet:%s", ref.ErrNotFound, id)
		}
		return ref.Node{}, err
	}
	st, err := loadState(fullID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ref.Node{}, fmt.Errorf("%w: meet:%s", ref.ErrNotFound, id)
		}
		return ref.Node{}, err
	}
	n := ref.NewNode(ref.Meet, st.ID)
	n.Title = strings.TrimSpace(st.Name)
	if n.Title == "" {
		n.Title = strings.TrimSpace(st.Topic)
	}
	n.Status = st.Status
	if where, err := baseDir(); err == nil {
		n.Where = where
	}
	n.Open = "bashy meet show " + st.ID
	return n, nil
}

func notFoundMeetingError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no meetings on this host") ||
		strings.Contains(msg, "nobody is in room") ||
		strings.Contains(msg, "no meeting matches")
}
