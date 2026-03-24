package storage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalFS is a Storage implementation backed by the local filesystem.
// It is intended for development and as a placeholder until the proprietary
// storage library is integrated.
//
// Layout under RootDir:
//
//	<sha256(key)>       — file content
//	<sha256(key)>.meta  — JSON-encoded Metadata
type LocalFS struct {
	root string
}

// NewLocalFS creates a LocalFS rooted at dir, creating it if necessary.
func NewLocalFS(dir string) (*LocalFS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create root dir %q: %w", dir, err)
	}
	return &LocalFS{root: dir}, nil
}

// keyToPath maps an arbitrary cache key to a stable filesystem path.
// SHA-256 hex avoids special characters and collisions.
func (fs *LocalFS) keyToPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(fs.root, fmt.Sprintf("%x", sum))
}

func (fs *LocalFS) Read(key string) (io.ReadCloser, *Metadata, error) {
	p := fs.keyToPath(key)
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("storage: key not found: %s", key)
		}
		return nil, nil, fmt.Errorf("storage: open %s: %w", key, err)
	}

	meta, err := fs.readMeta(p)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, meta, nil
}

func (fs *LocalFS) Write(key string, r io.Reader, meta *Metadata) error {
	p := fs.keyToPath(key)

	// Write content to a temp file first, then rename (atomic on most OS).
	tmp, err := os.CreateTemp(fs.root, ".tmp-")
	if err != nil {
		return fmt.Errorf("storage: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op if rename succeeded
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("storage: write content: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close temp: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("storage: rename: %w", err)
	}

	// Write metadata.
	if err := fs.writeMeta(p, meta); err != nil {
		os.Remove(p)
		return err
	}
	return nil
}

func (fs *LocalFS) Delete(key string) error {
	p := fs.keyToPath(key)
	err := os.Remove(p)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("storage: delete %s: %w", key, err)
	}
	_ = os.Remove(p + ".meta")
	return nil
}

func (fs *LocalFS) Exists(key string) bool {
	_, err := os.Stat(fs.keyToPath(key))
	return err == nil
}

func (fs *LocalFS) Stat(key string) (*FileInfo, error) {
	info, err := os.Stat(fs.keyToPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("storage: key not found: %s", key)
		}
		return nil, fmt.Errorf("storage: stat %s: %w", key, err)
	}
	return &FileInfo{Size: info.Size(), ModTime: info.ModTime()}, nil
}

// List returns keys whose SHA-256 path starts with prefix.
// Since keys are stored as opaque hashes, List walks all entries and returns
// those whose original key (stored in .meta) has the given prefix.
// For the flush-rule use case the prefix is always "" or a domain prefix,
// so we use the stored CustomMeta["key"] field when present.
func (fs *LocalFS) List(prefix string) ([]string, error) {
	entries, err := os.ReadDir(fs.root)
	if err != nil {
		return nil, fmt.Errorf("storage: list dir: %w", err)
	}

	var keys []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".meta") || strings.HasPrefix(e.Name(), ".tmp") {
			continue
		}
		metaPath := filepath.Join(fs.root, e.Name()+".meta")
		meta, err := fs.readMeta(filepath.Join(fs.root, e.Name()))
		if err != nil || meta == nil {
			continue
		}
		_ = metaPath
		origKey, ok := meta.CustomMeta["key"]
		if !ok {
			continue
		}
		if strings.HasPrefix(origKey, prefix) {
			keys = append(keys, origKey)
		}
	}
	return keys, nil
}

// ── meta helpers ──────────────────────────────────────────────────────────────

func (fs *LocalFS) readMeta(contentPath string) (*Metadata, error) {
	data, err := os.ReadFile(contentPath + ".meta")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Metadata{}, nil
		}
		return nil, fmt.Errorf("storage: read meta: %w", err)
	}
	var m Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("storage: decode meta: %w", err)
	}
	return &m, nil
}

func (fs *LocalFS) writeMeta(contentPath string, meta *Metadata) error {
	if meta == nil {
		meta = &Metadata{}
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("storage: encode meta: %w", err)
	}
	if err := os.WriteFile(contentPath+".meta", data, 0o644); err != nil {
		return fmt.Errorf("storage: write meta: %w", err)
	}
	return nil
}
