package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/guoyyit/bipartite-matching-go/matching"
)

const (
	walFileName    = "wal.log"
	tmpSuffix      = ".tmp"
	defaultHistory = 128
)

// ErrVersionNotFound is returned/used when a requested historical version is
// no longer (or not yet) available.
var ErrVersionNotFound = errors.New("version not found")

// VersionRecord is one committed, fully consistent version.
type VersionRecord struct {
	Version   int64
	Changes   []matching.Change
	Solutions map[matching.Scope]*matching.Solution
	Errors    map[matching.Scope]matching.FailureCode
	Graph     *matching.Graph
}

// walEntry is the on-disk JSON framing for one committed version.
type walEntry struct {
	Version   int64                        `json:"version"`
	Changes   []matching.Change            `json:"changes"`
	Solutions map[string]matching.Solution `json:"solutions"`
	Errors    map[string]string            `json:"errors,omitempty"`
	Graph     graphDTO                     `json:"graph"`
}

type graphDTO struct {
	Left      []string        `json:"left"`
	Right     []string        `json:"right"`
	Edges     []edgeDTO       `json:"edges"`
	Forbidden []matching.Pair `json:"forbidden,omitempty"`
	LeftCap   map[string]int  `json:"left_cap,omitempty"`
	RightCap  map[string]int  `json:"right_cap,omitempty"`
}

type edgeDTO struct {
	Left   string `json:"l"`
	Right  string `json:"r"`
	Weight int64  `json:"w"`
}

func toDTO(g *matching.Graph) graphDTO {
	dto := graphDTO{
		Left:     append([]string(nil), g.Left...),
		Right:    append([]string(nil), g.Right...),
		LeftCap:  g.LeftCap,
		RightCap: g.RightCap,
	}
	pairs := make([]matching.Pair, 0, len(g.Edges))
	for pair := range g.Edges {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Left != pairs[j].Left {
			return pairs[i].Left < pairs[j].Left
		}
		return pairs[i].Right < pairs[j].Right
	})
	for _, pair := range pairs {
		dto.Edges = append(dto.Edges, edgeDTO{pair.Left, pair.Right, g.Edges[pair]})
	}
	for pair := range g.Forbidden {
		dto.Forbidden = append(dto.Forbidden, pair)
	}
	sort.Slice(dto.Forbidden, func(i, j int) bool {
		if dto.Forbidden[i].Left != dto.Forbidden[j].Left {
			return dto.Forbidden[i].Left < dto.Forbidden[j].Left
		}
		return dto.Forbidden[i].Right < dto.Forbidden[j].Right
	})
	return dto
}

func fromDTO(dto graphDTO) *matching.Graph {
	g := matching.NewGraph()
	g.Left = append([]string(nil), dto.Left...)
	g.Right = append([]string(nil), dto.Right...)
	for _, e := range dto.Edges {
		g.Edges[matching.Pair{Left: e.Left, Right: e.Right}] = e.Weight
	}
	for _, pair := range dto.Forbidden {
		g.Forbidden[pair] = struct{}{}
	}
	for vertex, capValue := range dto.LeftCap {
		g.LeftCap[vertex] = capValue
	}
	for vertex, capValue := range dto.RightCap {
		g.RightCap[vertex] = capValue
	}
	return g
}

func toEntry(rec VersionRecord) walEntry {
	entry := walEntry{
		Version:   rec.Version,
		Changes:   append([]matching.Change(nil), rec.Changes...),
		Solutions: map[string]matching.Solution{},
		Graph:     toDTO(rec.Graph),
	}
	for scope, sol := range rec.Solutions {
		entry.Solutions[scopeName(scope)] = *sol
	}
	for scope, code := range rec.Errors {
		if entry.Errors == nil {
			entry.Errors = map[string]string{}
		}
		entry.Errors[scopeName(scope)] = string(code)
	}
	return entry
}

func fromEntry(entry walEntry) VersionRecord {
	rec := VersionRecord{
		Version:   entry.Version,
		Changes:   append([]matching.Change(nil), entry.Changes...),
		Solutions: map[matching.Scope]*matching.Solution{},
		Graph:     fromDTO(entry.Graph),
	}
	for name, sol := range entry.Solutions {
		scope := scopeFromName(name)
		sol := sol
		rec.Solutions[scope] = &sol
	}
	for name, code := range entry.Errors {
		if rec.Errors == nil {
			rec.Errors = map[matching.Scope]matching.FailureCode{}
		}
		rec.Errors[scopeFromName(name)] = matching.FailureCode(code)
	}
	return rec
}

func scopeName(scope matching.Scope) string { return scope.String() }

func scopeFromName(name string) matching.Scope {
	switch name {
	case matching.ScopeMaxCardinality.String():
		return matching.ScopeMaxCardinality
	case matching.ScopePerfect.String():
		return matching.ScopePerfect
	default:
		return matching.ScopeMaxWeight
	}
}

// Store is a local WAL/snapshot-backed persistent store.
type Store struct {
	mu      sync.RWMutex
	dir     string
	wal     *os.File
	records []VersionRecord
	latest  int64
}

// Open opens (or creates) a store in dir and replays any committed history.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	s := &Store{dir: dir, latest: -1}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// Append atomically persists one committed version and makes it visible.
func (s *Store) Append(rec VersionRecord) error {
	data, err := json.Marshal(toEntry(rec))
	if err != nil {
		return fmt.Errorf("encode version %d: %w", rec.Version, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Framed record: 8-byte big-endian length + JSON payload + newline.
	frame := make([]byte, 0, len(data)+9)
	frame = appendUint64(frame, uint64(len(data)))
	frame = append(frame, data...)
	frame = append(frame, '\n')

	if err := s.ensureWal(); err != nil {
		return err
	}
	if _, err := s.wal.Write(frame); err != nil {
		return fmt.Errorf("write version %d: %w", rec.Version, err)
	}
	if err := s.wal.Sync(); err != nil {
		return fmt.Errorf("fsync version %d: %w", rec.Version, err)
	}

	s.records = append(s.records, rec)
	s.latest = rec.Version
	if len(s.records) > 2*defaultHistory {
		// Compaction is best-effort and never blocks the commit path.
		_ = s.compactLocked(defaultHistory)
	}
	return nil
}

// Latest returns the newest committed version.
func (s *Store) Latest() VersionRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.records) == 0 {
		return VersionRecord{Version: -1}
	}
	return s.records[len(s.records)-1]
}

// At returns a committed historical version.
func (s *Store) At(version int64) (VersionRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.records) == 0 {
		return VersionRecord{}, false
	}
	base := s.records[0].Version
	if version < base || version > s.latest {
		return VersionRecord{}, false
	}
	return s.records[version-base], true
}

// Close flushes and releases the store.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wal != nil {
		if err := s.wal.Sync(); err != nil {
			return err
		}
		return s.wal.Close()
	}
	return nil
}

func appendUint64(buf []byte, value uint64) []byte {
	for shift := 56; shift >= 0; shift -= 8 {
		buf = append(buf, byte(value>>shift))
	}
	return buf
}

func (s *Store) ensureWal() error {
	if s.wal != nil {
		return nil
	}
	path := filepath.Join(s.dir, walFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open wal: %w", err)
	}
	s.wal = file
	return nil
}

// recover replays the WAL, discarding any trailing torn frame left by a crash.
func (s *Store) recover() error {
	path := filepath.Join(s.dir, walFileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open wal for recovery: %w", err)
	}
	s.wal = file

	info, err := file.Stat()
	if err != nil {
		return err
	}
	validBytes := int64(0)
	buf := make([]byte, info.Size())
	if len(buf) > 0 {
		if _, err := io.ReadFull(file, buf); err != nil {
			return fmt.Errorf("read wal: %w", err)
		}
	}

	for offset := int64(0); offset < int64(len(buf)); {
		if int64(len(buf)-int(offset)) < 9 {
			break // torn length header
		}
		length := int64(0)
		for i := 0; i < 8; i++ {
			length = length<<8 | int64(buf[int(offset)+i])
		}
		end := offset + 8 + length + 1
		if end > int64(len(buf)) {
			break // torn payload
		}
		if buf[end-1] != '\n' {
			break
		}
		var entry walEntry
		if err := json.Unmarshal(buf[offset+8:end-1], &entry); err != nil {
			break
		}
		s.records = append(s.records, fromEntry(entry))
		s.latest = entry.Version
		validBytes = end
		offset = end
	}

	if validBytes != int64(len(buf)) {
		// Truncate the torn tail so future appends start on a clean frame.
		if err := file.Truncate(validBytes); err != nil {
			return fmt.Errorf("truncate torn wal: %w", err)
		}
		if _, err := file.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}
	return nil
}

// compactLocked rewrites the WAL keeping only the most recent keep records.
// Compaction writes a temp file, fsyncs it, then atomically renames it over
// the live WAL so a crash never loses committed versions.
func (s *Store) compactLocked(keep int) error {
	if len(s.records) <= keep {
		return nil
	}
	drop := len(s.records) - keep
	tmpPath := filepath.Join(s.dir, walFileName+tmpSuffix)
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	for _, rec := range s.records[drop:] {
		data, err := json.Marshal(toEntry(rec))
		if err != nil {
			tmp.Close()
			return err
		}
		frame := appendUint64(nil, uint64(len(data)))
		frame = append(frame, data...)
		frame = append(frame, '\n')
		if _, err := tmp.Write(frame); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(s.dir, walFileName)); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(s.dir, walFileName), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if s.wal != nil {
		_ = s.wal.Close()
	}
	s.wal = file
	s.records = append([]VersionRecord(nil), s.records[drop:]...)
	return nil
}
