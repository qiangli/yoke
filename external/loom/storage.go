package loom

import (
	"fmt"
	"os"
	"strings"
)

// DefaultStorePort is the one fixed local outpost port that routes
// <peer-id>.<sat-id>.localhost to that peer's `s3` mesh service.
const DefaultStorePort = 31890

// Storage points loom's [storage] at a peer's S3 gateway. Presigned
// (SERVE_DIRECT) URLs embed Host, so every member host must resolve it to the
// same peer: PeerStoreHost builds a stable RFC 6761 loopback name that each
// outpost routes by Host header (Host is passed through unchanged, which the
// SigV4 signature requires).
type Storage struct {
	Endpoint  string // host:port, normally PeerStoreEndpoint(...)
	AccessKey string
	SecretKey string
	Bucket    string // default "loom"
	Location  string // default "us-east-1"
	UseSSL    bool
	// NoServeDirect keeps downloads proxied through loom (default: direct).
	NoServeDirect bool
}

// PeerStoreHost is the stable store name for a peer within a satellite.
func PeerStoreHost(peerID, satID string) string {
	return strings.ToLower(peerID + "." + satID + ".localhost")
}

// PeerStoreEndpoint is PeerStoreHost plus the fixed outpost port.
func PeerStoreEndpoint(peerID, satID string, port int) string {
	if port == 0 {
		port = DefaultStorePort
	}
	return fmt.Sprintf("%s:%d", PeerStoreHost(peerID, satID), port)
}

// StorageFromEnv reads LOOM_S3_ENDPOINT (or LOOM_S3_PEER + LOOM_S3_SAT),
// LOOM_S3_ACCESS_KEY, LOOM_S3_SECRET_KEY, LOOM_S3_BUCKET. nil when unset.
func StorageFromEnv() *Storage {
	ep := os.Getenv("LOOM_S3_ENDPOINT")
	if ep == "" {
		peer, sat := os.Getenv("LOOM_S3_PEER"), os.Getenv("LOOM_S3_SAT")
		if peer == "" || sat == "" {
			return nil
		}
		ep = PeerStoreEndpoint(peer, sat, 0)
	}
	return &Storage{
		Endpoint:  ep,
		AccessKey: os.Getenv("LOOM_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("LOOM_S3_SECRET_KEY"),
		Bucket:    os.Getenv("LOOM_S3_BUCKET"),
	}
}

// ensureStorageSection reconciles Gitea's global [storage] section, which LFS,
// packages, attachments, avatars, repo archives and Actions logs/artifacts all
// inherit. A nil/empty Storage leaves the file untouched.
func ensureStorageSection(ini string, st *Storage) string {
	if st == nil || st.Endpoint == "" {
		return ini
	}
	bucket, loc := st.Bucket, st.Location
	if bucket == "" {
		bucket = "loom"
	}
	if loc == "" {
		loc = "us-east-1"
	}
	return ensureSectionKeys(ini, "storage", map[string]string{
		"STORAGE_TYPE":             "minio",
		"MINIO_ENDPOINT":           st.Endpoint,
		"MINIO_ACCESS_KEY_ID":      st.AccessKey,
		"MINIO_SECRET_ACCESS_KEY":  st.SecretKey,
		"MINIO_BUCKET":             bucket,
		"MINIO_LOCATION":           loc,
		"MINIO_USE_SSL":            fmt.Sprint(st.UseSSL),
		"MINIO_BUCKET_LOOKUP_TYPE": "path",
		"MINIO_CHECK_BUCKET":       "true",
		"SERVE_DIRECT":             fmt.Sprint(!st.NoServeDirect),
	})
}
