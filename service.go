package bipartite

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/guoyyit/bipartite-matching-go/matching"
	"github.com/guoyyit/bipartite-matching-go/store"
)

// ErrTxnRolledBack is returned when an update is discarded or reused.
var ErrTxnRolledBack = errors.New("transaction rolled back")

// ErrUnknownVersion is returned for snapshot reads outside retained history.
var ErrUnknownVersion = errors.New("unknown or pruned version")

// snapshot is one immutable committed version shared by all readers.
type snapshot struct {
	version   int64
	graph     *matching.Graph
	solutions map[matching.Scope]*matching.Solution
	errors    map[matching.Scope]matching.FailureCode
}

// Service is the versioned, transactional, concurrent matching service.
type Service struct {
	mu       sync.Mutex // serializes commits only
	storage  *store.Store
	head     atomic.Pointer[snapshot]
	versions map[int64]*snapshot
	verMu    sync.RWMutex
	warm     map[matching.Scope]*matching.WarmSolver
	nextVer  int64
}

// Txn stages uncommitted graph changes privately.
type Txn struct {
	service *Service
	staged  *matching.Graph
	closed  bool
}

// Open creates or recovers a service backed by dir.
func Open(dir string, g *matching.Graph) (*Service, error) {
	if g == nil {
		g = matching.NewGraph()
	}
	storage, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		storage:  storage,
		versions: map[int64]*snapshot{},
		warm: map[matching.Scope]*matching.WarmSolver{
			matching.ScopeMaxWeight:      matching.NewWarmSolver(matching.ScopeMaxWeight),
			matching.ScopeMaxCardinality: matching.NewWarmSolver(matching.ScopeMaxCardinality),
			matching.ScopePerfect:        matching.NewWarmSolver(matching.ScopePerfect),
		},
	}

	latest := storage.Latest()
	if latest.Version >= 0 {
		snap := &snapshot{
			version:   latest.Version,
			graph:     latest.Graph,
			solutions: latest.Solutions,
			errors:    latest.Errors,
		}
		s.head.Store(snap)
		s.versions[snap.version] = snap
		s.nextVer = snap.version + 1
		// Rebuild warm-solver state through committed history so the next
		// incremental update does not start cold.
		for version := int64(1); version <= snap.version; version++ {
			rec, ok := storage.At(version)
			if !ok {
				continue
			}
			for _, scope := range scopes() {
				_, _ = s.warm[scope].Apply(rec.Graph)
			}
		}
	} else {
		if err := s.publishInitial(g); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// BeginUpdate starts a private update transaction based on the latest commit.
func (s *Service) BeginUpdate() *Txn {
	base := s.head.Load()
	return &Txn{service: s, staged: base.graph.Clone()}
}

// Commit validates and atomically publishes the staged changes.
func (t *Txn) Commit() (int64, error) {
	if t.closed {
		return 0, ErrTxnRolledBack
	}
	t.closed = true
	return t.service.commit(t.staged)
}

// Rollback discards all staged changes.
func (t *Txn) Rollback() {
	t.closed = true
}

// Query returns the matching at version (0 means latest) for a scope. When
// constraints make the requested scope infeasible, an *matching.InfeasibleError
// is returned and no partial result is exposed.
func (s *Service) Query(scope matching.Scope, version int64) (*matching.Solution, error) {
	if scope < matching.ScopeMaxWeight || scope > matching.ScopePerfect {
		return nil, errors.New("unknown solve scope")
	}
	if version == 0 {
		snap := s.head.Load()
		return snapSolution(snap, scope)
	}
	s.verMu.RLock()
	snap, ok := s.versions[version]
	s.verMu.RUnlock()
	if !ok {
		return nil, ErrUnknownVersion
	}
	return snapSolution(snap, scope)
}

func snapSolution(snap *snapshot, scope matching.Scope) (*matching.Solution, error) {
	if sol := snap.solutions[scope]; sol != nil {
		return cloneSolution(sol), nil
	}
	if code := snap.errors[scope]; code != "" {
		return nil, &matching.InfeasibleError{Code: code, Message: "constraints make " + scope.String() + " infeasible at version " + itoa(snap.version)}
	}
	return nil, errors.New(scope.String() + " was not solved at version " + itoa(snap.version))
}

// GraphAt returns the committed graph snapshot for a version (0 = latest).
func (s *Service) GraphAt(version int64) (*matching.Graph, error) {
	if version == 0 {
		return s.head.Load().graph.Clone(), nil
	}
	s.verMu.RLock()
	snap, ok := s.versions[version]
	s.verMu.RUnlock()
	if !ok {
		return nil, ErrUnknownVersion
	}
	return snap.graph.Clone(), nil
}

// Version returns the current committed version number.
func (s *Service) Version() int64 {
	return s.head.Load().version
}

// Close persists state and shuts the service down.
func (s *Service) Close() error {
	return s.storage.Close()
}

// AddEdge stages a new edge; duplicate pairs are rejected before commit.
func (t *Txn) AddEdge(pair matching.Pair, weight int64) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if _, exists := t.staged.Edges[pair]; exists {
		return errors.New("edge already exists: " + pair.Left + "/" + pair.Right)
	}
	t.ensureVertex(pair.Left, true)
	t.ensureVertex(pair.Right, false)
	t.staged.Edges[pair] = weight
	return nil
}

// DeleteEdge stages removal of an edge.
func (t *Txn) DeleteEdge(pair matching.Pair) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if _, exists := t.staged.Edges[pair]; !exists {
		return errors.New("edge does not exist: " + pair.Left + "/" + pair.Right)
	}
	delete(t.staged.Edges, pair)
	return nil
}

// SetWeight stages an in-place weight modification.
func (t *Txn) SetWeight(pair matching.Pair, weight int64) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if _, exists := t.staged.Edges[pair]; !exists {
		return errors.New("edge does not exist: " + pair.Left + "/" + pair.Right)
	}
	t.staged.Edges[pair] = weight
	return nil
}

// Forbid stages a forbidden-pair constraint.
func (t *Txn) Forbid(pair matching.Pair) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	t.staged.Forbidden[pair] = struct{}{}
	return nil
}

// Allow stages removal of a forbidden-pair constraint.
func (t *Txn) Allow(pair matching.Pair) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if _, banned := t.staged.Forbidden[pair]; !banned {
		return errors.New("pair is not forbidden: " + pair.Left + "/" + pair.Right)
	}
	delete(t.staged.Forbidden, pair)
	return nil
}

// SetLeftCap stages a per-left-vertex match-count cap.
func (t *Txn) SetLeftCap(vertex string, cap int) error {
	return t.setCap(vertex, cap, true)
}

// SetRightCap stages a per-right-vertex match-count cap.
func (t *Txn) SetRightCap(vertex string, cap int) error {
	return t.setCap(vertex, cap, false)
}

func (t *Txn) setCap(vertex string, cap int, left bool) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if cap < 0 {
		return errors.New("match cap must be non-negative")
	}
	t.ensureVertex(vertex, left)
	if left {
		t.staged.LeftCap[vertex] = cap
	} else {
		t.staged.RightCap[vertex] = cap
	}
	return nil
}

func (t *Txn) ensureVertex(vertex string, left bool) {
	list := t.staged.Right
	if left {
		list = t.staged.Left
	}
	for _, existing := range list {
		if existing == vertex {
			return
		}
	}
	if left {
		t.staged.Left = append(t.staged.Left, vertex)
	} else {
		t.staged.Right = append(t.staged.Right, vertex)
	}
}

func (s *Service) publishInitial(g *matching.Graph) error {
	solutions, solveErrors := solveAll(s.warm, g)
	rec := store.VersionRecord{
		Version:   1,
		Solutions: solutions,
		Errors:    solveErrors,
		Graph:     g.Clone(),
	}
	if err := s.storage.Append(rec); err != nil {
		return err
	}
	s.install(&snapshot{version: 1, graph: g.Clone(), solutions: solutions, errors: solveErrors})
	s.nextVer = 2
	return nil
}

// commit serializes all updates: solves incrementally, persists first and
// only then publishes the new immutable snapshot to readers.
func (s *Service) commit(g *matching.Graph) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	solutions, solveErrors := solveAll(s.warm, g)
	version := s.nextVer
	rec := store.VersionRecord{
		Version:   version,
		Solutions: solutions,
		Errors:    solveErrors,
		Graph:     g.Clone(),
	}
	if err := s.storage.Append(rec); err != nil {
		return 0, err
	}
	s.install(&snapshot{version: version, graph: g.Clone(), solutions: solutions, errors: solveErrors})
	s.nextVer++
	return version, nil
}

func solveAll(warm map[matching.Scope]*matching.WarmSolver, g *matching.Graph) (map[matching.Scope]*matching.Solution, map[matching.Scope]matching.FailureCode) {
	solutions := map[matching.Scope]*matching.Solution{}
	solveErrors := map[matching.Scope]matching.FailureCode{}
	for _, scope := range scopes() {
		sol, err := warm[scope].Apply(g)
		if err != nil {
			var infeasible *matching.InfeasibleError
			if errors.As(err, &infeasible) {
				solveErrors[scope] = infeasible.Code
			}
			continue
		}
		solutions[scope] = sol
	}
	return solutions, solveErrors
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

func (s *Service) install(snap *snapshot) {
	s.head.Store(snap)
	s.verMu.Lock()
	s.versions[snap.version] = snap
	s.verMu.Unlock()
}

func scopes() []matching.Scope {
	return []matching.Scope{
		matching.ScopeMaxWeight,
		matching.ScopeMaxCardinality,
		matching.ScopePerfect,
	}
}

func cloneSolution(sol *matching.Solution) *matching.Solution {
	if sol == nil {
		return nil
	}
	cp := *sol
	cp.Matched = append([]matching.Match(nil), sol.Matched...)
	cp.UnmatchedLeft = append([]string(nil), sol.UnmatchedLeft...)
	cp.UnmatchedRight = append([]string(nil), sol.UnmatchedRight...)
	return &cp
}
