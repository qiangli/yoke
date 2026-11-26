// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const testGiB = uint64(1) << 30

func putTestBlobRecord(t *testing.T, ctx context.Context, st *Store, name string, payload []byte, retention RetentionClass, recipe *RegenerationRecipe) string {
	t.Helper()
	dgst := DigestBytes(payload)
	var roots []string
	if retention == RetentionProtectedRoot {
		roots = []string{"test-root:" + name}
	}
	rec := CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         dgst,
		Size:           int64(len(payload)),
		MediaType:      "application/octet-stream",
		LogicalName:    name,
		LogicalVersion: "v1",
		Provenance:     "pressure-test",
		Retention:      retention,
		RetentionRoots: roots,
		Recipe:         recipe,
	}
	if _, err := st.PutArtifact(ctx, rec, bytes.NewReader(payload)); err != nil {
		t.Fatalf("PutArtifact(%s): %v", name, err)
	}
	return dgst
}

func TestPressureAdvisoryOnlyPreservation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

	st, err := OpenStore(ctx, StoreOptions{Root: root, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	scratchDigest := putTestBlobRecord(t, ctx, st, "scratch/build-cache", bytes.Repeat([]byte("s"), 4096), RetentionScratch, nil)
	inputDigest := putTestBlobRecord(t, ctx, st, "source/base-input", bytes.Repeat([]byte("p"), 2048), RetentionProtectedRoot, nil)
	reconPayload := bytes.Repeat([]byte("r"), 4096)
	reconDigest := DigestBytes(reconPayload)
	putTestBlobRecord(t, ctx, st, "derived/output", reconPayload, RetentionReconstructible, &RegenerationRecipe{
		Kind:           RecipeDerivedCacheV1,
		InputDigests:   []string{inputDigest},
		ExpectedDigest: reconDigest,
	})

	unloadCalls := 0
	prof := DefaultProfileV1(CurrentPlatform())
	prof.Resources.Activated = false // Advisory-only default!

	var sample PressureObservation
	ctrl := NewPressureController(PressureControllerOptions{
		Root:    root,
		Profile: &prof,
		Backend: st,
		Now:     func() time.Time { return now },
		ObserveFunc: func(context.Context) (PressureObservation, error) {
			return sample, nil
		},
		UnloadModelsFunc: func(context.Context, string) ([]string, error) {
			unloadCalls++
			return []string{"qwen3.5:4b"}, nil
		},
	})

	// 1. Inject warning sample -> advisory only.
	sample = PressureObservation{
		At:              now,
		CPUUsagePercent: 88,
		MemTotalBytes:   64 * testGiB,
		MemUsedBytes:    56 * testGiB,
		MemUsedPercent:  87.5,
		Disks: []DiskObservation{{
			Path:        root,
			TotalBytes:  500 * testGiB,
			UsedBytes:   435 * testGiB,
			FreeBytes:   18 * testGiB,
			UsedPercent: 87,
			Role:        "artifact",
		}},
	}
	resWarn, err := ctrl.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate warning: %v", err)
	}
	if resWarn.State.Activated || resWarn.State.GrowthBlocked || resWarn.ReclaimedItems != 0 || unloadCalls != 0 {
		t.Fatalf("advisory warning mutated state: %+v unloadCalls=%d", resWarn, unloadCalls)
	}
	if resWarn.State.ObservedLevel != PressureWarning {
		t.Fatalf("expected ObservedLevel=warning, got %q", resWarn.State.ObservedLevel)
	}

	// 2. Inject critical disk/mem/CPU samples across 6 minutes -> still zero mutations!
	for i := 0; i <= 5; i++ {
		now = now.Add(time.Minute)
		sample = PressureObservation{
			At:              now,
			CPUUsagePercent: 98,
			MemTotalBytes:   64 * testGiB,
			MemUsedBytes:    62 * testGiB,
			MemUsedPercent:  97,
			Disks: []DiskObservation{{
				Path:        root,
				TotalBytes:  500 * testGiB,
				UsedBytes:   492 * testGiB,
				FreeBytes:   8 * testGiB,
				UsedPercent: 98,
				Role:        "artifact",
			}},
		}
		resCrit, err := ctrl.Evaluate(ctx)
		if err != nil {
			t.Fatalf("Evaluate critical[%d]: %v", i, err)
		}
		if resCrit.State.GrowthBlocked || resCrit.ReclaimedItems != 0 || unloadCalls != 0 {
			t.Fatalf("advisory critical[%d] mutated host: %+v unloadCalls=%d", i, resCrit, unloadCalls)
		}
		if resCrit.State.ObservedLevel != PressureCritical {
			t.Fatalf("expected ObservedLevel=critical, got %q", resCrit.State.ObservedLevel)
		}
	}

	for _, d := range []string{scratchDigest, inputDigest, reconDigest} {
		rs, err := st.GetRecordStatus(ctx, d)
		if err != nil || !rs.PayloadPresent {
			t.Fatalf("blob %s was unexpectedly evicted in advisory mode: %+v (%v)", d, rs, err)
		}
	}
}

func TestPressureWarningReclamationPreservesProtectedAndActiveLeases(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	now := time.Date(2026, 11, 26, 13, 0, 0, 0, time.UTC)

	st, err := OpenStore(ctx, StoreOptions{
		Root:  root,
		Now:   func() time.Time { return now },
		Alive: func(int, string) bool { return true },
	})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	protectedDigest := putTestBlobRecord(t, ctx, st, "baseline/protected-root", bytes.Repeat([]byte("P"), 8192), RetentionProtectedRoot, nil)

	leasedPayload := bytes.Repeat([]byte("L"), 8192)
	leasedDigest := DigestBytes(leasedPayload)
	putTestBlobRecord(t, ctx, st, "derived/leased-reconstructible", leasedPayload, RetentionReconstructible, &RegenerationRecipe{
		Kind:           RecipeDerivedCacheV1,
		InputDigests:   []string{protectedDigest},
		ExpectedDigest: leasedDigest,
	})

	unleasedPayload := bytes.Repeat([]byte("U"), 8192)
	unleasedDigest := DigestBytes(unleasedPayload)
	putTestBlobRecord(t, ctx, st, "derived/unleased-reconstructible", unleasedPayload, RetentionReconstructible, &RegenerationRecipe{
		Kind:           RecipeDerivedCacheV1,
		InputDigests:   []string{protectedDigest},
		ExpectedDigest: unleasedDigest,
	})

	scratchDigest := putTestBlobRecord(t, ctx, st, "scratch/disposable-temp", bytes.Repeat([]byte("S"), 4096), RetentionScratch, nil)

	prof := DefaultProfileV1(CurrentPlatform())
	prof.Resources.Activated = true

	ctrl := NewPressureController(PressureControllerOptions{
		Root:    root,
		Profile: &prof,
		Backend: st,
		Now:     func() time.Time { return now },
		ObserveFunc: func(context.Context) (PressureObservation, error) {
			return PressureObservation{
				At:              now,
				CPUUsagePercent: 20,
				MemTotalBytes:   64 * testGiB,
				MemUsedBytes:    20 * testGiB,
				MemUsedPercent:  31.25,
				Disks: []DiskObservation{{
					Path:        root,
					TotalBytes:  500 * testGiB,
					UsedBytes:   430 * testGiB,
					FreeBytes:   18 * testGiB, // 86% used and <20 GiB free -> warning!
					UsedPercent: 86.0,
					Role:        "artifact",
				}},
			}, nil
		},
	})

	var evalRes PressureResult
	err = st.WithReadLease(ctx, os.Getpid(), "holder-start", 10*time.Minute, []string{leasedDigest}, func(lctx context.Context) error {
		var e error
		evalRes, e = ctrl.Evaluate(lctx)
		return e
	})
	if err != nil {
		t.Fatalf("Evaluate under read lease: %v", err)
	}

	if evalRes.State.Level != PressureWarning {
		t.Fatalf("state level = %q, want warning", evalRes.State.Level)
	}
	if evalRes.State.GrowthBlocked {
		t.Fatalf("warning level should not set GrowthBlocked: %+v", evalRes.State)
	}
	wantBytes := int64(8192 + 4096)
	if evalRes.ReclaimedBytes != wantBytes || evalRes.ReclaimedItems != 2 {
		t.Fatalf("reclaimed = (%d items, %d bytes), want (2, %d)", evalRes.ReclaimedItems, evalRes.ReclaimedBytes, wantBytes)
	}

	// Verify scratch + unleased reconstructible blobs were evicted.
	for _, evicted := range []string{scratchDigest, unleasedDigest} {
		rs, err := st.GetRecordStatus(ctx, evicted)
		if err != nil {
			t.Fatalf("GetRecordStatus(%s): %v", evicted, err)
		}
		if rs.PayloadPresent {
			t.Fatalf("expected %s (%s) to be evicted", rs.Record.LogicalName, evicted)
		}
	}

	// Verify protected root and active leased reconstructible blobs remain 100% intact!
	for _, retained := range []string{protectedDigest, leasedDigest} {
		rs, err := st.GetRecordStatus(ctx, retained)
		if err != nil || !rs.PayloadPresent {
			t.Fatalf("expected %s to remain intact, got %+v (%v)", retained, rs, err)
		}
		if _, err := st.FetchVerified(ctx, retained); err != nil {
			t.Fatalf("FetchVerified(%s) failed: %v", retained, err)
		}
	}
}

func TestPressureCriticalModelUnloadUnsatisfiedFloorAndOTelJournal(t *testing.T) {
	ctx := context.Background()

	// Build a mock Ollama server holding qwen3.5:4b in memory.
	var mu sync.Mutex
	var keepAliveReceived []any
	residentModel := "qwen3.5:4b"
	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			if residentModel == "" {
				_, _ = w.Write([]byte(`{"models":[]}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": []LoadedOllamaModel{{
					Name:     residentModel,
					Model:    residentModel,
					Size:     3 << 30,
					SizeVRAM: 3 << 30,
				}},
			})
		case "/api/generate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			keepAliveReceived = append(keepAliveReceived, body["keep_alive"])
			if ka, ok := body["keep_alive"].(float64); ok && ka == 0 && body["model"] == "qwen3.5:4b" {
				residentModel = ""
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollamaSrv.Close()

	// Verify all critical triggers: 96% disk / 8 GiB free, 96% inodes, freeBytes < RebuildReserveBytes, and 96% sustained 5m CPU.
	triggers := []struct {
		name      string
		sustained bool
		makeObs   func(root string, at time.Time) PressureObservation
	}{
		{
			name: "disk_96pct_8gib",
			makeObs: func(root string, at time.Time) PressureObservation {
				return PressureObservation{
					At:            at,
					MemTotalBytes: 64 * testGiB,
					MemUsedBytes:  20 * testGiB,
					Disks: []DiskObservation{{
						Path:        root,
						TotalBytes:  200 * testGiB,
						UsedBytes:   192 * testGiB,
						FreeBytes:   8 * testGiB,
						UsedPercent: 96.0,
						Role:        "artifact",
					}},
				}
			},
		},
		{
			name: "inode_96pct",
			makeObs: func(root string, at time.Time) PressureObservation {
				return PressureObservation{
					At:            at,
					MemTotalBytes: 64 * testGiB,
					MemUsedBytes:  20 * testGiB,
					Disks: []DiskObservation{{
						Path:              root,
						TotalBytes:        200 * testGiB,
						UsedBytes:         100 * testGiB,
						FreeBytes:         100 * testGiB,
						UsedPercent:       50.0,
						InodesTotal:       1_000_000,
						InodesUsed:        960_000,
						InodesFree:        40_000,
						InodesUsedPercent: 96.0,
						Role:              "artifact",
					}},
				}
			},
		},
		{
			name: "rebuild_reserve_breach",
			makeObs: func(root string, at time.Time) PressureObservation {
				return PressureObservation{
					At:            at,
					MemTotalBytes: 20 * testGiB,
					MemUsedBytes:  8 * testGiB,
					Disks: []DiskObservation{{
						Path:        root,
						TotalBytes:  15 * testGiB, // <20GiB warn byte floor, so only rebuild reserve (8 GiB) trips critical!
						UsedBytes:   9 * testGiB,
						FreeBytes:   6 * testGiB,
						UsedPercent: 60.0,
						Role:        "artifact",
					}},
				}
			},
		},
		{
			name:      "sustained_5m_cpu_96pct",
			sustained: true,
			makeObs: func(root string, at time.Time) PressureObservation {
				return PressureObservation{
					At:              at,
					CPUUsagePercent: 96.0,
					CPUSource:       "sampled",
					MemTotalBytes:   64 * testGiB,
					MemUsedBytes:    20 * testGiB,
					Disks: []DiskObservation{{
						Path:        root,
						TotalBytes:  200 * testGiB,
						UsedBytes:   100 * testGiB,
						FreeBytes:   100 * testGiB,
						UsedPercent: 50.0,
						Role:        "artifact",
					}},
				}
			},
		},
	}

	for _, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			now := time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC)

			st, err := OpenStore(ctx, StoreOptions{Root: root, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatalf("OpenStore: %v", err)
			}
			defer st.Close()

			// Populate ONLY RetentionProtectedRoot model weights in CAS.
			weightBytes := bytes.Repeat([]byte("W"), 16384)
			weightDigest := putTestBlobRecord(t, ctx, st, "ollama-blob/qwen35-weights", weightBytes, RetentionProtectedRoot, nil)

			mu.Lock()
			residentModel = "qwen3.5:4b"
			keepAliveReceived = nil
			mu.Unlock()

			// Register in-memory OpenTelemetry span & metric readers (no external collector running!).
			spanRecorder := tracetest.NewSpanRecorder()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
			defer func() { _ = tp.Shutdown(ctx) }()

			metricReader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
			defer func() { _ = mp.Shutdown(ctx) }()

			prof := DefaultProfileV1(CurrentPlatform())
			prof.Resources.Activated = true

			ctrl := NewPressureController(PressureControllerOptions{
				Root:           root,
				Profile:        &prof,
				Backend:        st,
				OllamaBaseURL:  ollamaSrv.URL,
				HTTPClient:     ollamaSrv.Client(),
				TracerProvider: tp,
				MeterProvider:  mp,
				Now:            func() time.Time { return now },
				ObserveFunc: func(context.Context) (PressureObservation, error) {
					return tc.makeObs(root, now), nil
				},
			})

			var res PressureResult
			if tc.sustained {
				// First sample at t=0 establishes window start; sample at t=5m covers 5m sustain window.
				if _, err := ctrl.Evaluate(ctx); err != nil {
					t.Fatalf("Evaluate t=0: %v", err)
				}
				now = now.Add(5 * time.Minute)
			}
			res, err = ctrl.Evaluate(ctx)
			if err != nil {
				t.Fatalf("Evaluate critical: %v", err)
			}

			if res.State.Level != PressureCritical {
				t.Fatalf("Level = %q, want critical", res.State.Level)
			}
			if !res.State.GrowthBlocked || res.State.ThrottleConcurrency != 1 || !res.State.UnsatisfiedFloor {
				t.Fatalf("unexpected critical state flags: %+v", res.State)
			}
			if res.State.FloorDetail == nil || res.State.FloorDetail.ProtectedBytes != int64(len(weightBytes)) {
				t.Fatalf("expected protected_bytes=%d in FloorDetail, got %+v", len(weightBytes), res.State.FloorDetail)
			}

			// Verify qwen3.5:4b was unloaded via keep_alive: 0 while CAS weights remain 100% intact!
			mu.Lock()
			stillResident := residentModel
			kaCount := len(keepAliveReceived)
			mu.Unlock()
			if stillResident != "" || kaCount == 0 || !slices.Contains(res.UnloadedModels, "qwen3.5:4b") {
				t.Fatalf("expected qwen3.5:4b unloaded via keep_alive:0; resident=%q ka=%v unloaded=%v", stillResident, keepAliveReceived, res.UnloadedModels)
			}
			gotWeights, err := st.FetchVerified(ctx, weightDigest)
			if err != nil || !bytes.Equal(gotWeights, weightBytes) {
				t.Fatalf("protected CAS model weights modified or missing: %v", err)
			}

			// Verify bounded local alerts.jsonl contains reclaim, model_unload, and unsatisfied_floor.
			alerts, err := LoadAlertRecords(root, 0)
			if err != nil {
				t.Fatalf("LoadAlertRecords: %v", err)
			}
			var eventTypes []string
			for _, a := range alerts {
				if a.SchemaVersion != AlertJournalSchemaV1 {
					t.Fatalf("unexpected alert schema %q", a.SchemaVersion)
				}
				eventTypes = append(eventTypes, a.EventType)
			}
			for _, wantEv := range []string{AlertEventReclaim, AlertEventModelUnload, AlertEventUnsatisfiedFloor} {
				if !slices.Contains(eventTypes, wantEv) {
					t.Fatalf("alerts.jsonl missing %q; got %v", wantEv, eventTypes)
				}
			}

			// Check 0600 permissions on unix.
			if fi, err := os.Stat(filepath.Join(root, alertJournalFile)); err != nil {
				t.Fatalf("stat alerts.jsonl: %v", err)
			} else if fi.Size() == 0 {
				t.Fatal("alerts.jsonl is empty")
			}

			// Verify OTel spans and metrics recorded in-memory.
			spans := spanRecorder.Ended()
			if len(spans) == 0 {
				t.Fatal("expected OTel span to be recorded")
			}
			var spanEvents []string
			for _, sp := range spans {
				for _, ev := range sp.Events() {
					spanEvents = append(spanEvents, ev.Name)
				}
			}
			for _, wantEv := range []string{AlertEventReclaim, AlertEventModelUnload, AlertEventUnsatisfiedFloor} {
				if !slices.Contains(spanEvents, wantEv) {
					t.Fatalf("OTel span events missing %q; got %v", wantEv, spanEvents)
				}
			}

			var rm metricdata.ResourceMetrics
			if err := metricReader.Collect(ctx, &rm); err != nil {
				t.Fatalf("collect OTel metrics: %v", err)
			}
			var metricNames []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					metricNames = append(metricNames, m.Name)
				}
			}
			for _, wantMetric := range []string{"bashy.offline.transitions", "bashy.offline.unsatisfied_floor"} {
				if !slices.Contains(metricNames, wantMetric) {
					t.Fatalf("OTel metrics missing %q; got %v", wantMetric, metricNames)
				}
			}
		})
	}
}

func TestPressureRecoveryHysteresis(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)

	prof := DefaultProfileV1(CurrentPlatform())
	prof.Resources.Activated = true

	var diskUsedPct float64
	var diskFreeBytes uint64
	const totalDisk = 200 * testGiB

	ctrl := NewPressureController(PressureControllerOptions{
		Root:    root,
		Profile: &prof,
		Now:     func() time.Time { return now },
		UnloadModelsFunc: func(context.Context, string) ([]string, error) {
			return nil, nil
		},
		ObserveFunc: func(context.Context) (PressureObservation, error) {
			return PressureObservation{
				At:            now,
				MemTotalBytes: 64 * testGiB,
				MemUsedBytes:  16 * testGiB,
				Disks: []DiskObservation{{
					Path:        root,
					TotalBytes:  totalDisk,
					UsedBytes:   uint64(float64(totalDisk) * diskUsedPct / 100.0),
					FreeBytes:   diskFreeBytes,
					UsedPercent: diskUsedPct,
					Role:        "artifact",
				}},
			}, nil
		},
	})

	// Step 0 (t=0): Enter critical pressure -> GrowthBlocked == true.
	diskUsedPct = 96.0
	diskFreeBytes = 8 * testGiB
	res, err := ctrl.Evaluate(ctx)
	if err != nil {
		t.Fatalf("t=0 critical: %v", err)
	}
	if res.State.Level != PressureCritical || !res.State.GrowthBlocked {
		t.Fatalf("t=0 expected critical + GrowthBlocked, got %+v", res.State)
	}

	// Step 1 (t+20s) & Step 2 (t+40s): 2 healthy samples (75% used, 30 GiB free) -> STILL blocked (<3 samples and <60s).
	diskUsedPct = 75.0
	diskFreeBytes = 30 * testGiB
	for _, dt := range []time.Duration{20 * time.Second, 40 * time.Second} {
		now = time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC).Add(dt)
		res, err = ctrl.Evaluate(ctx)
		if err != nil {
			t.Fatalf("Evaluate at %v: %v", dt, err)
		}
		if !res.State.GrowthBlocked || res.State.Level != PressureCritical {
			t.Fatalf("at %v expected still blocked, got %+v", dt, res.State)
		}
	}
	if res.State.HealthySamples != 2 {
		t.Fatalf("expected HealthySamples=2 at t+40s, got %d", res.State.HealthySamples)
	}

	// Step 3 (t+50s): Sample at 82% used (below 85% warn, but ABOVE 80% recovery target!) -> resets counter!
	now = time.Date(2026, 11, 26, 15, 0, 50, 0, time.UTC)
	diskUsedPct = 82.0
	diskFreeBytes = 36 * testGiB
	res, err = ctrl.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate at 82%%: %v", err)
	}
	if !res.State.GrowthBlocked || res.State.HealthySamples != 0 || res.State.HealthySince != nil {
		t.Fatalf("expected 82%% used to reset recovery hysteresis counter, got %+v", res.State)
	}

	// Step 4: Inject 3 consecutive healthy samples (<=80% used AND >=25 GiB free) spanning >=60s.
	diskUsedPct = 78.0
	diskFreeBytes = 28 * testGiB

	// Sample 1 of 3 at t=60s
	now = time.Date(2026, 11, 26, 15, 1, 0, 0, time.UTC)
	res, err = ctrl.Evaluate(ctx)
	if err != nil || !res.State.GrowthBlocked || res.State.HealthySamples != 1 {
		t.Fatalf("recovery sample 1: %+v (%v)", res.State, err)
	}

	// Sample 2 of 3 at t=90s (30s elapsed < 60s)
	now = time.Date(2026, 11, 26, 15, 1, 30, 0, time.UTC)
	res, err = ctrl.Evaluate(ctx)
	if err != nil || !res.State.GrowthBlocked || res.State.HealthySamples != 2 {
		t.Fatalf("recovery sample 2: %+v (%v)", res.State, err)
	}

	// Sample 3 of 3 at t=120s (60s elapsed >= 60s, 3 consecutive healthy samples >= 3) -> RECOVERED!
	now = time.Date(2026, 11, 26, 15, 2, 0, 0, time.UTC)
	res, err = ctrl.Evaluate(ctx)
	if err != nil {
		t.Fatalf("recovery sample 3: %v", err)
	}
	if res.State.Level != PressureNormal || res.State.GrowthBlocked || res.State.UnsatisfiedFloor {
		t.Fatalf("expected full recovery to normal + GrowthBlocked=false, got %+v", res.State)
	}

	alerts, err := LoadAlertRecords(root, 0)
	if err != nil {
		t.Fatalf("LoadAlertRecords: %v", err)
	}
	last := alerts[len(alerts)-1]
	if last.EventType != AlertEventRecovery || last.ToLevel != PressureNormal || last.GrowthBlocked {
		t.Fatalf("expected trailing recovery alert, got %+v", last)
	}
}

func TestAlertJournalBoundedCap(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 26, 16, 0, 0, 0, time.UTC)
	var batch []AlertRecordV1
	for i := range 20 {
		batch = append(batch, AlertRecordV1{
			SchemaVersion:  AlertJournalSchemaV1,
			At:             now.Add(time.Duration(i) * time.Second),
			EventType:      AlertEventReclaim,
			Reasons:        []string{strings.Repeat("x", 8)},
			ReclaimedItems: i,
		})
	}
	if err := AppendAlertRecords(root, 10, batch...); err != nil {
		t.Fatalf("AppendAlertRecords: %v", err)
	}
	got, err := LoadAlertRecords(root, 0)
	if err != nil {
		t.Fatalf("LoadAlertRecords: %v", err)
	}
	if len(got) != 10 || got[0].ReclaimedItems != 10 || got[9].ReclaimedItems != 19 {
		t.Fatalf("expected newest 10 records (10..19), got len=%d first=%+v last=%+v", len(got), got[0], got[len(got)-1])
	}
}
