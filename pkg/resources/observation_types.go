package resources

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const HostObservationSchema = "bashy-host-observation-v1"
const AlertStateSchema = "bashy-resource-alert-state-v1"
const HostObservationTTL = 5 * time.Second
const DirectoryObservationTTL = 60 * time.Second

type ObservationStatus struct {
	Kind        string    `json:"kind"`
	Stale       bool      `json:"stale"`
	Source      string    `json:"source"`
	Reason      string    `json:"reason,omitempty"`
	At          time.Time `json:"at"`
	ExpiresAt   time.Time `json:"expires_at"`
	WindowStart time.Time `json:"window_start,omitempty"`
	WindowEnd   time.Time `json:"window_end,omitempty"`
}
type ObservationValue[T any] struct {
	Value  *T                `json:"value"`
	Status ObservationStatus `json:"status"`
}
type ObservationCoverage struct {
	Complete bool   `json:"complete"`
	Seen     int    `json:"seen"`
	Included int    `json:"included"`
	Limit    int    `json:"limit"`
	Reason   string `json:"reason,omitempty"`
}
type ProcessIdentity struct {
	PID     int    `json:"pid"`
	StartID string `json:"start_id"`
}
type ProcessObservation struct {
	Identity    ProcessIdentity           `json:"identity"`
	PPID        int                       `json:"ppid"`
	Name        string                    `json:"name"`
	CPU         ObservationValue[float64] `json:"cpu"`
	RSS         ObservationValue[uint64]  `json:"rss"`
	WorkloadID  string                    `json:"workload_id,omitempty"`
	Attribution string                    `json:"attribution"`
	Reason      string                    `json:"reason,omitempty"`
}
type WorkloadRef struct {
	ID           string          `json:"id"`
	Agent        string          `json:"agent"`
	Sprint       string          `json:"sprint"`
	Run          string          `json:"run"`
	Workspace    string          `json:"workspace"`
	Root         ProcessIdentity `json:"root"`
	RegisteredAt time.Time       `json:"registered_at"`
}
type WorkloadObservation struct {
	WorkloadRef
	ProcessCount int                       `json:"process_count"`
	CPU          ObservationValue[float64] `json:"cpu"`
	RSS          ObservationValue[uint64]  `json:"rss"`
	Coverage     ObservationCoverage       `json:"coverage"`
}
type ScanRoot struct {
	Path       string `json:"path"`
	WorkloadID string `json:"workload_id"`
	Kind       string `json:"kind"`
}
type DirectoryObservation struct {
	Path                 string                    `json:"path"`
	WorkloadID           string                    `json:"workload_id"`
	Kind                 string                    `json:"kind"`
	Bytes                ObservationValue[uint64]  `json:"bytes"`
	GrowthBytesPerSecond ObservationValue[float64] `json:"growth_bytes_per_second"`
	Entries              int                       `json:"entries"`
	Coverage             ObservationCoverage       `json:"coverage"`
}
type HostObservation struct {
	SchemaVersion     string                       `json:"schema_version"`
	ID                string                       `json:"id"`
	At                time.Time                    `json:"at"`
	ExpiresAt         time.Time                    `json:"expires_at"`
	System            *System                      `json:"system"`
	Sections          map[string]ObservationStatus `json:"sections"`
	Processes         []ProcessObservation         `json:"processes"`
	Directories       []DirectoryObservation       `json:"directories"`
	Workloads         []WorkloadObservation        `json:"workloads"`
	ProcessCoverage   ObservationCoverage          `json:"process_coverage"`
	DirectoryCoverage ObservationCoverage          `json:"directory_coverage"`
	Refreshing        bool                         `json:"refreshing"`
}
type HostObserveOptions struct {
	CacheDir  string
	Workloads []WorkloadRef
	ScanRoots []ScanRoot
}

// AlertLedger contains derived bookkeeping only. The caller owns the versioned
// condition/outbox payload and its delivery semantics; persistence sends nothing.
type AlertLedger struct {
	SchemaVersion string                     `json:"schema_version"`
	Revision      uint64                     `json:"revision"`
	UpdatedAt     time.Time                  `json:"updated_at"`
	Entries       map[string]json.RawMessage `json:"entries"`
}

func ResourcesStateDir() string {
	if p := strings.TrimSpace(os.Getenv("BASHY_RESOURCES_DIR")); p != "" {
		return p
	}
	if p := strings.TrimSpace(os.Getenv("BASHY_HOME")); p != "" {
		return filepath.Join(p, "resources")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bashy", "resources")
}
func observationStatus(kind, source, reason string, at time.Time, ttl time.Duration) ObservationStatus {
	return ObservationStatus{Kind: kind, Source: source, Reason: reason, At: at, ExpiresAt: at.Add(ttl)}
}
func observedValue[T any](v T, source string, at time.Time, ttl time.Duration) ObservationValue[T] {
	return ObservationValue[T]{Value: &v, Status: observationStatus("actual", source, "", at, ttl)}
}
func unknownValue[T any](source, reason string, at time.Time) ObservationValue[T] {
	return ObservationValue[T]{Status: observationStatus("unknown", source, reason, at, HostObservationTTL)}
}
