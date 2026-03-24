package storage

import "time"

// Metadata holds HTTP metadata associated with a cached file.
// CustomMeta carries cache-internal fields (e.g. WrittenAt, TTL).
type Metadata struct {
	ContentType  string
	CacheControl string
	Expires      string
	CustomMeta   map[string]string
}

// FileInfo describes a stored object.
type FileInfo struct {
	Size    int64
	ModTime time.Time
}
