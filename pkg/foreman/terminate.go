package foreman

import (
	"errors"
	"os"
)

// ErrServeNotProven is Terminate's refusal: the recorded serve process could
// not be proven to be this session's, so nothing was signalled.
var ErrServeNotProven = errors.New("foreman: serve process is not proven to own this session")

// withServeProcess stamps the calling process as st's serve process.
func withServeProcess(st State) State {
	st.ServePID = os.Getpid()
	st.ServeStart, _ = ProcessStart(st.ServePID)
	return st
}
