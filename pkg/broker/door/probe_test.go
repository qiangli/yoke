package door

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func slowModelsDoor(t *testing.T, delay time.Duration) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pid":7,"local_engine":true,"engine_up":true}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"x_kind":"local"},{"x_kind":"agent"},{"x_kind":"agent"}]}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv(PortEnv, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
}

func TestProbeSlowListingStillCounts(t *testing.T) {
	slowModelsDoor(t, ProbeTimeout+500*time.Millisecond)
	st, err := Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Up || !st.CountsKnown || st.LocalModels != 1 || st.AgentEntries != 2 || st.Error != "" {
		t.Fatalf("slow listing must still be counted: %+v", st)
	}
}

func TestProbeTimedOutCountsAreUnknown(t *testing.T) {
	slowModelsDoor(t, 2*time.Second)
	old := CountsTimeout
	CountsTimeout = 200 * time.Millisecond
	t.Cleanup(func() { CountsTimeout = old })
	st, err := Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Up || st.CountsKnown || st.Error == "" {
		t.Fatalf("timed-out counts must be reported unknown: %+v", st)
	}
}
