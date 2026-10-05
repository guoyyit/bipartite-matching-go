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

// ErrConflict is returned when a transaction is based on a committed version
// that is no longer the latest and its staged operations cannot be replayed
// safely over the newer state (for example it modifies an edge that a
// concurrent transaction deleted). It is distinct from every other error and
// is always retryable: callers must begin a fresh transaction from the
// latest version, reapply their changes and commit again. A rejected
// transaction has published and persisted nothing and can never be committed
// afterwards.
var ErrConflict = errors.New("commit conflict: base version is stale")

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
	mu      sync.Mutex // serializes commits only
	storage *store.Store
	head    atomic.Pointer[snapshot]
	warm    map[matching.Scope]*matching.WarmSolver
}

// Txn stages uncommitted graph changes privately.
type Txn struct {
	service *Service
	staged  *matching.Graph
	base    int64 // committed version the private copy was taken from
	changes []matching.Change
	closed  bool
}

// Open creates or recovers a service backed by dir.
func Open(dir string, g *matching.Graph, opts ...store.Option) (*Service, error) {
	if g == nil {
		g = matching.NewGraph()
	}
	storage, err := store.Open(dir, opts...)
	if err != nil {
		return nil, err
	}
	s := &Service{
		storage: storage,
		warm: map[matching.Scope]*matching.WarmSolver{
			matching.ScopeMaxWeight:      matching.NewWarmSolver(matching.ScopeMaxWeight),
			matching.ScopeMaxCardinality: matching.NewWarmSolver(matching.ScopeMaxCardinality),
			matching.ScopePerfect:        matching.NewWarmSolver(matching.ScopePerfect),
		},
	}

	latest := storage.Latest()
	if latest.Version >= 0 {
		s.install(&snapshot{
			version:   latest.Version,
			graph:     latest.Graph,
			solutions: latest.Solutions,
			errors:    latest.Errors,
		})
		// Rebuild warm-solver state from committed history so the first
		// post-restart update stays incremental.
		s.rebuildWarm()
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
	return &Txn{service: s, staged: base.graph.Clone(), base: base.version}
}

// Commit validates and atomically publishes the staged changes.
func (t *Txn) Commit() (int64, error) {
	if t.closed {
		return 0, ErrTxnRolledBack
	}
	t.closed = true
	return t.service.commit(t.staged, t.base, t.changes)
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
	return s.solutionAt(version, scope)
}

func (s *Service) solutionAt(version int64, scope matching.Scope) (*matching.Solution, error) {
	rec, ok := s.storage.At(version)
	if !ok {
		return nil, ErrUnknownVersion
	}
	snap := &snapshot{
		version:   rec.Version,
		graph:     rec.Graph,
		solutions: rec.Solutions,
		errors:    rec.Errors,
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
	rec, ok := s.storage.At(version)
	if !ok {
		return nil, ErrUnknownVersion
	}
	return rec.Graph.Clone(), nil
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
	t.changes = append(t.changes, matching.Change{Kind: matching.ChangeAddEdge, Pair: pair, Weight: weight})
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
	t.changes = append(t.changes, matching.Change{Kind: matching.ChangeDeleteEdge, Pair: pair})
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
	t.changes = append(t.changes, matching.Change{Kind: matching.ChangeSetWeight, Pair: pair, Weight: weight})
	return nil
}

// Forbid stages a forbidden-pair constraint.
func (t *Txn) Forbid(pair matching.Pair) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	t.staged.Forbidden[pair] = struct{}{}
	t.changes = append(t.changes, matching.Change{Kind: matching.ChangeForbid, Pair: pair})
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
	t.changes = append(t.changes, matching.Change{Kind: matching.ChangeAllow, Pair: pair})
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
	kind := matching.ChangeSetRightCap
	if left {
		kind = matching.ChangeSetLeftCap
	}
	t.changes = append(t.changes, matching.Change{Kind: kind, Vertex: vertex, Cap: cap})
	return nil
}

func (t *Txn) ensureVertex(vertex string, left bool) {
	ensureVertex(t.staged, vertex, left)
}

func (s *Service) publishInitial(g *matching.Graph) error {
	solutions, solveErrors := solveAll(s.warm, g)
	rec := store.VersionRecord{
		Version:   1,
		Changes:   nil,
		Solutions: solutions,
		Errors:    solveErrors,
		Graph:     g.Clone(),
	}
	if err := s.storage.Append(rec); err != nil {
		return err
	}
	s.install(&snapshot{version: 1, graph: g.Clone(), solutions: solutions, errors: solveErrors})
	return nil
}

// commit serializes all updates. If the transaction is based on the current
// head, its private graph is committed directly. Otherwise the committed
// operations are replayed in order over the newest committed graph: changes
// to distinct edges/constraints all survive, and writes to the same key take
// the later committer's value, exactly as if the transaction had run serially
// after the concurrent commits. When a precondition of a replayed operation
// has been invalidated by an intervening commit (e.g. the edge it modifies
// was deleted), the commit is rejected with ErrConflict without persisting
// or publishing anything. The new version is fully persisted before readers
// can observe it.
func (s *Service) commit(staged *matching.Graph, base int64, changes []matching.Change) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head := s.head.Load()
	g := staged
	if base != head.version {
		merged, err := replay(head.graph.Clone(), changes)
		if err != nil {
			return 0, err
		}
		g = merged
	}

	solutions, solveErrors := solveAll(s.warm, g)
	version := head.version + 1
	rec := store.VersionRecord{
		Version:   version,
		Changes:   append([]matching.Change(nil), changes...),
		Solutions: solutions,
		Errors:    solveErrors,
		Graph:     g.Clone(),
	}
	if err := s.storage.Append(rec); err != nil {
		// Persistence failed; the warm solvers have already advanced onto g.
		// Rebuild them from the durable committed history so the next
		// successful commit still increments bit-for-bit correctly.
		s.rebuildWarm()
		return 0, err
	}
	s.install(&snapshot{version: version, graph: g.Clone(), solutions: solutions, errors: solveErrors})
	return version, nil
}

// replay applies a transaction's operations in their original order onto a
// graph that already contains later commits. Operations whose precondition
// the concurrent commits invalidated produce ErrConflict; the caller then
// rejects the whole commit, so no partial state can escape.
func replay(g *matching.Graph, changes []matching.Change) (*matching.Graph, error) {
	for _, change := range changes {
		switch change.Kind {
		case matching.ChangeAddEdge:
			if _, exists := g.Edges[change.Pair]; exists {
				return nil, ErrConflict
			}
			ensureVertex(g, change.Pair.Left, true)
			ensureVertex(g, change.Pair.Right, false)
			g.Edges[change.Pair] = change.Weight
		case matching.ChangeDeleteEdge:
			if _, exists := g.Edges[change.Pair]; !exists {
				return nil, ErrConflict
			}
			delete(g.Edges, change.Pair)
		case matching.ChangeSetWeight:
			if _, exists := g.Edges[change.Pair]; !exists {
				return nil, ErrConflict
			}
			g.Edges[change.Pair] = change.Weight
		case matching.ChangeForbid:
			g.Forbidden[change.Pair] = struct{}{}
		case matching.ChangeAllow:
			if _, banned := g.Forbidden[change.Pair]; !banned {
				return nil, ErrConflict
			}
			delete(g.Forbidden, change.Pair)
		case matching.ChangeSetLeftCap:
			ensureVertex(g, change.Vertex, true)
			g.LeftCap[change.Vertex] = change.Cap
		case matching.ChangeSetRightCap:
			ensureVertex(g, change.Vertex, false)
			g.RightCap[change.Vertex] = change.Cap
		default:
			return nil, errors.New("unknown staged change")
		}
	}
	return g, nil
}

func ensureVertex(g *matching.Graph, vertex string, left bool) {
	list := g.Right
	if left {
		list = g.Left
	}
	for _, existing := range list {
		if existing == vertex {
			return
		}
	}
	if left {
		g.Left = append(g.Left, vertex)
	} else {
		g.Right = append(g.Right, vertex)
	}
}

// rebuildWarm resets every warm solver and replays the retained durable
// history, so incremental solving continues from the exact committed state.
func (s *Service) rebuildWarm() {
	s.warm = map[matching.Scope]*matching.WarmSolver{
		matching.ScopeMaxWeight:      matching.NewWarmSolver(matching.ScopeMaxWeight),
		matching.ScopeMaxCardinality: matching.NewWarmSolver(matching.ScopeMaxCardinality),
		matching.ScopePerfect:        matching.NewWarmSolver(matching.ScopePerfect),
	}
	oldest, ok := s.storage.Oldest()
	if !ok {
		return
	}
	latest := s.storage.Latest().Version
	for version := oldest; version <= latest; version++ {
		rec, ok := s.storage.At(version)
		if !ok {
			continue
		}
		for _, scope := range scopes() {
			_, _ = s.warm[scope].Apply(rec.Graph)
		}
	}
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
