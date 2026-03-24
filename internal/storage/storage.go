// Package storage defines the abstraction over the underlying file storage.
// The actual implementation is either the local filesystem (LocalFS) or
// a proprietary library injected at startup. No other package in this service
// should access the filesystem directly.
package storage

import "io"

// Storage is the interface every storage backend must satisfy.
type Storage interface {
	// Read returns the content of key. Caller must close the returned reader.
	Read(key string) (io.ReadCloser, *Metadata, error)

	// Write stores r under key with the given metadata.
	// The implementation may buffer the full body before writing.
	Write(key string, r io.Reader, meta *Metadata) error

	// Delete removes key. Returns nil if key did not exist.
	Delete(key string) error

	// Exists reports whether key is present without reading its content.
	Exists(key string) bool

	// Stat returns size and modification time for key.
	Stat(key string) (*FileInfo, error)

	// List returns all keys that start with prefix.
	// Used by the flush rule engine to enumerate cache entries.
	List(prefix string) ([]string, error)
}
