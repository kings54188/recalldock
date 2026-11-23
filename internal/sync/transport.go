package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrChunkNotFound = errors.New("sync: chunk not found")

// Transport defines how chunks are read and written during sync.
// This is the abstraction that allows the same Syncer to work with
// both local filesystem (.recalldock/ directory) and remote cloud server.
type Transport interface {
	// ReadManifest returns the manifest (chunk index).
	// Returns an empty manifest if none exists yet.
	ReadManifest() (*Manifest, error)

	// WriteManifest persists the manifest.
	WriteManifest(m *Manifest) error

	// WriteChunk writes a compressed chunk to the transport.
	// chunkID is the content-addressed ID (8 hex chars).
	// data is the raw JSON bytes (FileTransport gzips them; remote sends as-is).
	// entry contains metadata about the chunk.
	WriteChunk(chunkID string, data []byte, entry ChunkEntry) error

	// ReadChunk reads a compressed chunk from the transport.
	// Returns the raw bytes (gzipped for FileTransport, JSON for remote).
	ReadChunk(chunkID string) ([]byte, error)
}

// ─── FileTransport ──────────────────────────────────────────────────────────

// FileTransport reads/writes chunks to the local filesystem.
// This encapsulates all filesystem operations that were previously
// inline in the Syncer methods.
type FileTransport struct {
	syncDir string // Path to .recalldock/ directory
}

// NewFileTransport creates a FileTransport rooted at the given sync directory.
func NewFileTransport(syncDir string) *FileTransport {
	return &FileTransport{syncDir: syncDir}
}

// validateDirectoryChain distinguishes a genuinely missing path from a path
// hidden behind a regular-file parent. Windows reports both as not-exist.
func validateDirectoryChain(path string) error {
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", current)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// ReadManifest reads the manifest.json from the sync directory.
// Returns an empty manifest (Version=1) if the file does not exist.
func (ft *FileTransport) ReadManifest() (*Manifest, error) {
	f, err := os.OpenInRoot(ft.syncDir, "manifest.json")
	if err != nil {
		if os.IsNotExist(err) {
			if dirErr := validateDirectoryChain(ft.syncDir); dirErr != nil {
				return nil, fmt.Errorf("read manifest: %w", dirErr)
			}
			return &Manifest{Version: 1}, nil
		}
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	defer f.Close()
	data, err := readLimitedJSON(f)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// WriteManifest writes the manifest to manifest.json in the sync directory.
func (ft *FileTransport) WriteManifest(m *Manifest) error {
	data, err := jsonMarshalManifest(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := checkJSONSize(data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	root, err := os.OpenRoot(ft.syncDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile("manifest.json", data, 0644)
}

// WriteChunk writes gzipped chunk data to the chunks/ subdirectory.
func (ft *FileTransport) WriteChunk(chunkID string, data []byte, _ ChunkEntry) error {
	if err := validateChunkID(chunkID); err != nil {
		return err
	}
	if err := checkJSONSize(data); err != nil {
		return err
	}
	if err := os.MkdirAll(ft.syncDir, 0755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}
	root, err := os.OpenRoot(ft.syncDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll("chunks", 0755); err != nil {
		return fmt.Errorf("create chunks dir: %w", err)
	}
	f, err := root.OpenFile(filepath.Join("chunks", chunkID+".jsonl.gz"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeGzipFile(f, data)
}

// ReadChunk reads gzipped chunk data from the chunks/ subdirectory.
func (ft *FileTransport) ReadChunk(chunkID string) ([]byte, error) {
	if err := validateChunkID(chunkID); err != nil {
		return nil, err
	}
	chunksDir := filepath.Join(ft.syncDir, "chunks")
	f, err := os.OpenInRoot(ft.syncDir, filepath.Join("chunks", chunkID+".jsonl.gz"))
	if err != nil {
		if os.IsNotExist(err) {
			if dirErr := validateDirectoryChain(chunksDir); dirErr != nil {
				return nil, fmt.Errorf("read chunk: %w", dirErr)
			}
			return nil, ErrChunkNotFound
		}
		return nil, err
	}
	defer f.Close()
	return readGzipStream(f)
}

func validateChunkID(id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("sync: invalid chunk ID %q", id)
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			continue
		}
		return fmt.Errorf("sync: invalid chunk ID %q", id)
	}
	return nil
}
