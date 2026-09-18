package spacetime

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTimeNamespaceIsVolatileAndChanges(t *testing.T) {
	oldNow := timeNow
	defer func() { timeNow = oldNow }()

	current := time.Date(2026, time.July, 27, 7, 15, 0, 0, time.FixedZone("Morning/Office", -7*60*60))
	timeNow = func() time.Time { return current }
	ps := DefaultProbes(NewFileCache(t.TempDir(), 24*time.Hour))
	defer ps.Close()

	before := ps.Snapshot([]string{"time.hour", "time.weekday", "time.zone", "time.attended"})
	current = time.Date(2026, time.July, 28, 14, 15, 0, 0, time.FixedZone("Afternoon/Cafe", 2*60*60))
	after := ps.Snapshot([]string{"time.hour", "time.weekday", "time.zone", "time.attended"})

	wantBefore := map[string]string{
		"time.hour": "07", "time.weekday": "monday",
		"time.zone": "morning-office", "time.attended": "false",
	}
	wantAfter := map[string]string{
		"time.hour": "14", "time.weekday": "tuesday",
		"time.zone": "afternoon-cafe", "time.attended": "true",
	}
	assertProbeValues(t, before, wantBefore)
	assertProbeValues(t, after, wantAfter)
	if ContextKey(before) == ContextKey(after) {
		t.Fatal("time coordinate did not change its ContextKey")
	}
}

func TestRequiresAcceptsNumericTimeValues(t *testing.T) {
	if _, err := ParseRequires("time.hour=07,19 time.weekday=monday"); err != nil {
		t.Fatalf("numeric time values must be gateable: %v", err)
	}
}

func TestPlaceIDChangesWithoutLeakingRawNetworkSignals(t *testing.T) {
	oldSignals := placeSignals
	defer func() { placeSignals = oldSignals }()

	rawSSID := "Cafe WiFi Secret"
	rawMAC := "aa:bb:cc:dd:ee:ff"
	rawIP := "192.0.2.44"
	var mu sync.Mutex
	current := []string{"ssid:" + rawSSID, "gateway-mac:" + rawMAC, "peer-ip:" + rawIP}
	placeSignals = func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), current...), nil
	}

	ps := DefaultProbes(NopCache())
	defer ps.Close()
	first, ok := ps.Value("place.id")
	if !ok {
		t.Fatal("place.id was absent")
	}
	mu.Lock()
	current = []string{"ssid:Office", "gateway-mac:11:22:33:44:55:66"}
	mu.Unlock()
	second, ok := ps.Value("place.id")
	if !ok || first == second {
		t.Fatalf("place.id did not change across simulated movement: %q -> %q", first, second)
	}

	encoded, err := json.Marshal(map[string]string{"place.id": first})
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded) + first + second
	for _, secret := range []string{rawSSID, rawMAC, rawIP} {
		if strings.Contains(output, secret) {
			t.Fatalf("probe output leaked raw network signal %q: %s", secret, output)
		}
	}
}

func TestLocalityObservationDebouncesFlaps(t *testing.T) {
	lan, calls := true, 0
	events := make(chan []string, 2)
	ps := DefaultProbes(NopCache())
	ps.Register(netResolver{sameLAN: &lan, calls: &calls})
	ps.movementDebounce = 25 * time.Millisecond
	ps.publishMovement = func(changed []string) error {
		events <- append([]string(nil), changed...)
		return nil
	}
	defer ps.Close()

	ps.Value("net.same_lan")
	lan = false
	ps.Forget("net.same_lan")
	ps.Value("net.same_lan")
	lan = true
	ps.Forget("net.same_lan")
	ps.Value("net.same_lan")
	time.Sleep(40 * time.Millisecond)
	select {
	case event := <-events:
		t.Fatalf("locality flap published an event: %v", event)
	default:
	}

	lan = false
	ps.Forget("net.same_lan")
	ps.Value("net.same_lan")
	select {
	case event := <-events:
		if len(event) != 1 || event[0] != "net.same_lan" {
			t.Fatalf("locality event = %v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("stable locality observation was not published")
	}
}

func TestMovementMonitorDebouncesFlapsAndPublishesStableChange(t *testing.T) {
	oldSignals := placeSignals
	defer func() { placeSignals = oldSignals }()

	var mu sync.Mutex
	current := "gateway-mac:aa:aa:aa:aa:aa:aa"
	var samples atomic.Int32
	placeSignals = func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		samples.Add(1)
		return []string{current}, nil
	}
	setPlace := func(value string) {
		mu.Lock()
		current = value
		mu.Unlock()
	}

	events := make(chan []string, 4)
	ps := DefaultProbes(NopCache())
	ps.movementPoll = 5 * time.Millisecond
	ps.movementDebounce = 30 * time.Millisecond
	ps.publishMovement = func(changed []string) error {
		events <- append([]string(nil), changed...)
		return nil
	}
	defer ps.Close()

	if _, ok := ps.Value("place.id"); !ok {
		t.Fatal("place.id was absent")
	}
	ps.StartMovementMonitor()
	deadline := time.Now().Add(time.Second)
	for samples.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	// A brief association flap returns to baseline inside the stability
	// window and must not publish.
	setPlace("gateway-mac:bb:bb:bb:bb:bb:bb")
	time.Sleep(10 * time.Millisecond)
	setPlace("gateway-mac:aa:aa:aa:aa:aa:aa")
	time.Sleep(50 * time.Millisecond)
	select {
	case event := <-events:
		t.Fatalf("flapping association published an event: %v", event)
	default:
	}

	// A stable simulated roam is announced exactly once.
	setPlace("gateway-mac:cc:cc:cc:cc:cc:cc")
	select {
	case event := <-events:
		if len(event) != 1 || event[0] != "place.id" {
			t.Fatalf("movement event = %v, want [place.id]", event)
		}
	case <-time.After(time.Second):
		t.Fatal("stable place change was not published")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case event := <-events:
		t.Fatalf("stable transition published more than once: %v", event)
	default:
	}
}

func assertProbeValues(t *testing.T, got, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}
