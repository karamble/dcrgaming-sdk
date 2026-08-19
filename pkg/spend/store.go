package spend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// fileStore keeps the book in one JSON file, written whole.
//
// Written whole rather than appended because the book is small - a table's
// worth of requests - and a whole-file write through a temp and a rename is the
// one shape that cannot leave a half-written record behind. The same shape
// pkg/identity uses for the seed, and for the same reason.
type fileStore struct {
	mu   sync.Mutex
	path string
}

// FileStore keeps the book at a path, creating its directory if needed.
//
// This is the default and the supported path: the runtime owns the writes and a
// game cannot corrupt the record by getting its own persistence wrong.
func FileStore(path string) (Store, error) {
	if path == "" {
		return nil, fmt.Errorf("a file store needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("make the spend book's directory: %w", err)
	}
	return &fileStore{path: path}, nil
}

type bookFile struct {
	Records []Record `json:"records"`
}

func (s *fileStore) Load() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f bookFile
	if err := json.Unmarshal(raw, &f); err != nil {
		// A book that will not parse is refused rather than started
		// empty. Starting empty would silently stop watching every
		// payment it held.
		return nil, fmt.Errorf("the spend book at %s is unreadable: %w", s.path, err)
	}
	return f.Records, nil
}

func (s *fileStore) Save(rs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out, err := json.MarshalIndent(bookFile{Records: rs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// MemStore keeps a book in memory. For tests, and for a game that has genuinely
// decided its money record need not survive a restart - which is almost never.
func MemStore() Store { return &memStore{} }

type memStore struct {
	mu sync.Mutex
	rs []Record
}

func (s *memStore) Load() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.rs...), nil
}

func (s *memStore) Save(rs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rs = append([]Record(nil), rs...)
	return nil
}
