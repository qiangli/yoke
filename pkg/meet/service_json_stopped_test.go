package meet

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// `meet service status --json` prints the bashy-meet-service-v1 envelope and
// exits 0 when the daemon is stopped: a supervisor polling JSON must not need
// a non-zero exit to learn the state. Human mode keeps the non-zero exit.
func TestServiceStatusJSONStoppedExitsZero(t *testing.T) {
	serviceTestDir(t)
	port := freeServicePort(t)
	out, err := runMeet(t, "service", "status", "--json", "--port", strconv.Itoa(port))
	if err != nil {
		t.Fatalf("stopped status --json must exit 0; err = %v", err)
	}
	var st ServiceStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("stopped status --json must emit one JSON object, got %q: %v", out, err)
	}
	if st.SchemaVersion != serviceSchema {
		t.Fatalf("schema_version = %q, want %q", st.SchemaVersion, serviceSchema)
	}
	if st.Running {
		t.Fatalf("stopped status must report running=false, got %+v", st)
	}
}

// When serviceStatusOf itself errors (an unidentified listener holds the port),
// --json must still fail: the envelope cannot answer what was not identified.
func TestServiceStatusJSONUnidentifiedStillFails(t *testing.T) {
	serviceTestDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not a meet server"))
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	_, err := runMeet(t, "service", "status", "--json", "--port", strconv.Itoa(port))
	if !errors.Is(err, ErrServiceUnidentified) {
		t.Fatalf("unidentified listener must fail even under --json; err = %v", err)
	}
}
