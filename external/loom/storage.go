package loom

import (
	"fmt"
	"os"
	"strings"
)

// DefaultStorePort is the one fixed local outpost port that routes
// /s3-<peer-id>/... to that peer's `s3` mesh service.
const DefaultStorePort = 31890

// Storage points loom's [storage] at a peer's S3 gateway. The peer is named by
// the bucket (s3-<peer-id>), not by a DNS name: the endpoint is always the
// loopback outpost store port, so presigned (SERVE_DIRECT) URLs resolve on
// every OS without hosts/DNS changes and outpost routes by the path prefix.
type Storage struct {
	Endpoint  string // host:port, normally PeerStoreEndpoint(...)
	AccessKey string
	SecretKey string
	Bucket    string // default "loom"; normally PeerStoreBucket(peer)
	Location  string // default "us-east-1"
	UseSSL    bool
	// NoServeDirect keeps downloads proxied through loom (default: direct).
	NoServeDirect bool
}

// PeerStoreBucket is the stable bucket (and outpost route prefix) for a peer.
func PeerStoreBucket(peerID string) string {
	return "s3-" + strings.ToLower(peerID)
}

// PeerStoreEndpoint is the loopback outpost store address.
func PeerStoreEndpoint(port int) string {
	if port == 0 {
		port = DefaultStorePort
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// StorageFromEnv reads LOOM_S3_PEER (bucket s3-<peer>, endpoint 127.0.0.1:
// <store port>), LOOM_S3_ENDPOINT (explicit override), LOOM_S3_ACCESS_KEY,
// LOOM_S3_SECRET_KEY, LOOM_S3_BUCKET (explicit override). nil when neither
// LOOM_S3_ENDPOINT nor LOOM_S3_PEER is set.
func StorageFromEnv() *Storage {
	peer := os.Getenv("LOOM_S3_PEER")
	ep := os.Getenv("LOOM_S3_ENDPOINT")
	if ep == "" {
		if peer == "" {
			return nil
		}
		ep = PeerStoreEndpoint(0)
	}
	bucket := os.Getenv("LOOM_S3_BUCKET")
	if bucket == "" && peer != "" {
		bucket = PeerStoreBucket(peer)
	}
	return &Storage{
		Endpoint:  ep,
		AccessKey: os.Getenv("LOOM_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("LOOM_S3_SECRET_KEY"),
		Bucket:    bucket,
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
