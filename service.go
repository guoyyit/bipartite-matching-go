package bipartite

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/guoyyit/bipartite-matching-go/matching"
	"github.com/guoyyit/bipartite-matching-go/store"
)

// ErrTxnRolledBack is returned when an update is discarded or reused.
var ErrTxnRolledBack = errors.New("transaction rolled back")

// ErrUnknownVersion is returned for snapshot reads outside retained history.
var ErrUnknownVersion = errors.New("unknown or pruned version")

// ErrTxnConflict rejects a commit whose transaction was based on a stale
// version and whose changes cannot be replayed cleanly onto the latest
// committed state (for example it re-adds an edge another commit already
// added, or reweights an edge another commit deleted). It is distinguishable
// via errors.Is and retryable: open a fresh transaction on the latest
// version and re-apply the changes. A rejected transaction never commits.
var ErrTxnConflict = errors.New("transaction conflicts with committed state")

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
	service    *Service
	base       int64
	staged     *matching.Graph
	ops        []matching.Change
	closed     bool
	conflicted bool
}

// Open creates or recovers a service backed by dir.
func Open(dir string, g *matching.Graph) (*Service, error) {
	return OpenWithHistoryLimit(dir, g, 0)
}

// OpenWithHistoryLimit opens a service that retains between historyLimit and
// 2*historyLimit committed versions for historical reads (a non-positive
// limit selects the default). The limit applies identically before and after
// a restart: the readable window always matches the records retained on disk.
func OpenWithHistoryLimit(dir string, g *matching.Graph, historyLimit int) (*Service, error) {
	if g == nil {
		g = matching.NewGraph()
	}
	storage, err := store.OpenWithHistoryLimit(dir, historyLimit)
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

	records := storage.Records()
	if len(records) > 0 {
		// Every retained committed version stays readable after a restart,
		// and warm-solver state is rebuilt through them so the next
		// incremental update does not start cold.
		for _, rec := range records {
			snap := &snapshot{
				version:   rec.Version,
				graph:     rec.Graph,
				solutions: rec.Solutions,
				errors:    rec.Errors,
			}
			s.versions[rec.Version] = snap
			for _, scope := range scopes() {
				_, _ = s.warm[scope].Apply(rec.Graph)
			}
		}
		latest := records[len(records)-1]
		s.head.Store(s.versions[latest.Version])
		s.nextVer = latest.Version + 1
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
	return &Txn{service: s, base: base.version, staged: base.graph.Clone()}
}

// Commit validates and atomically publishes the staged changes. When other
// transactions committed after this one began, the staged operations are
// replayed onto the latest committed state; the result is then exactly the
// serial application of this transaction after those commits. If any
// operation no longer applies, the commit is rejected with ErrTxnConflict
// and the transaction cannot be committed again.
func (t *Txn) Commit() (int64, error) {
	if t.closed {
		if t.conflicted {
			return 0, ErrTxnConflict
		}
		return 0, ErrTxnRolledBack
	}
	t.closed = true
	version, err := t.service.commit(t)
	if err != nil && errors.Is(err, ErrTxnConflict) {
		t.conflicted = true
	}
	return version, err
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
	change := matching.Change{Kind: matching.ChangeAddEdge, Pair: pair, Weight: weight}
	return t.stage(change)
}

// DeleteEdge stages removal of an edge.
func (t *Txn) DeleteEdge(pair matching.Pair) error {
	return t.stage(matching.Change{Kind: matching.ChangeDeleteEdge, Pair: pair})
}

// SetWeight stages an in-place weight modification.
func (t *Txn) SetWeight(pair matching.Pair, weight int64) error {
	return t.stage(matching.Change{Kind: matching.ChangeSetWeight, Pair: pair, Weight: weight})
}

// Forbid stages a forbidden-pair constraint.
func (t *Txn) Forbid(pair matching.Pair) error {
	return t.stage(matching.Change{Kind: matching.ChangeForbid, Pair: pair})
}

// Allow stages removal of a forbidden-pair constraint.
func (t *Txn) Allow(pair matching.Pair) error {
	return t.stage(matching.Change{Kind: matching.ChangeAllow, Pair: pair})
}

// SetLeftCap stages a per-left-vertex match-count cap.
func (t *Txn) SetLeftCap(vertex string, cap int) error {
	return t.stage(matching.Change{Kind: matching.ChangeSetLeftCap, Pair: matching.Pair{Left: vertex}, Cap: cap})
}

// SetRightCap stages a per-right-vertex match-count cap.
func (t *Txn) SetRightCap(vertex string, cap int) error {
	return t.stage(matching.Change{Kind: matching.ChangeSetRightCap, Pair: matching.Pair{Right: vertex}, Cap: cap})
}

// stage validates one change against the private staged graph and records it
// in the transaction journal for a potential replay at commit time.
func (t *Txn) stage(change matching.Change) error {
	if t.closed {
		return ErrTxnRolledBack
	}
	if err := applyChange(t.staged, change); err != nil {
		return err
	}
	t.ops = append(t.ops, change)
	return nil
}

// applyChange applies one journaled change to g with the same validation the
// staging path uses. It is shared by staging and by commit-time replay, so a
// replayed transaction observes exactly the serial application of its ops.
func applyChange(g *matching.Graph, change matching.Change) error {
	pair := change.Pair
	switch change.Kind {
	case matching.ChangeAddEdge:
		if _, exists := g.Edges[pair]; exists {
			return errors.New("edge already exists: " + pair.Left + "/" + pair.Right)
		}
		ensureVertex(g, pair.Left, true)
		ensureVertex(g, pair.Right, false)
		g.Edges[pair] = change.Weight
	case matching.ChangeDeleteEdge:
		if _, exists := g.Edges[pair]; !exists {
			return errors.New("edge does not exist: " + pair.Left + "/" + pair.Right)
		}
		delete(g.Edges, pair)
	case matching.ChangeSetWeight:
		if _, exists := g.Edges[pair]; !exists {
			return errors.New("edge does not exist: " + pair.Left + "/" + pair.Right)
		}
		g.Edges[pair] = change.Weight
	case matching.ChangeForbid:
		g.Forbidden[pair] = struct{}{}
	case matching.ChangeAllow:
		if _, banned := g.Forbidden[pair]; !banned {
			return errors.New("pair is not forbidden: " + pair.Left + "/" + pair.Right)
		}
		delete(g.Forbidden, pair)
	case matching.ChangeSetLeftCap, matching.ChangeSetRightCap:
		if change.Cap < 0 {
			return errors.New("match cap must be non-negative")
		}
		left := change.Kind == matching.ChangeSetLeftCap
		vertex := pair.Right
		if left {
			vertex = pair.Left
		}
		ensureVertex(g, vertex, left)
		if left {
			g.LeftCap[vertex] = change.Cap
		} else {
			g.RightCap[vertex] = change.Cap
		}
	default:
		return errors.New("unknown change kind")
	}
	return nil
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

// commit serializes all updates: it merges the transaction onto the latest
// committed state, solves incrementally, persists first and only then
// publishes the new immutable snapshot to readers. A transaction based on a
// stale version is rejected with ErrTxnConflict when its journal cannot be
// replayed cleanly; a rejected commit assigns no version and publishes
// nothing.
func (s *Service) commit(t *Txn) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head := s.head.Load()
	graph := t.staged
	if t.base != head.version {
		// The base is stale: replay the journal onto the latest committed
		// state. Success is exactly the serial application of this
		// transaction after the intervening commits; any operation that no
		// longer applies rejects the commit with a retryable conflict.
		graph = head.graph.Clone()
		for _, op := range t.ops {
			if err := applyChange(graph, op); err != nil {
				return 0, fmt.Errorf("%w: %v", ErrTxnConflict, err)
			}
		}
	}

	solutions, solveErrors := solveAll(s.warm, graph)
	version := s.nextVer
	rec := store.VersionRecord{
		Version:   version,
		Changes:   append([]matching.Change(nil), t.ops...),
		Solutions: solutions,
		Errors:    solveErrors,
		Graph:     graph.Clone(),
	}
	if err := s.storage.Append(rec); err != nil {
		return 0, err
	}
	s.install(&snapshot{version: version, graph: graph, solutions: solutions, errors: solveErrors})
	s.nextVer++
	s.pruneVersions()
	return version, nil
}

// pruneVersions drops in-memory snapshots the store has compacted away, so
// the readable history window is identical before and after a restart.
func (s *Service) pruneVersions() {
	base, ok := s.storage.Base()
	if !ok {
		return
	}
	s.verMu.Lock()
	defer s.verMu.Unlock()
	for version := range s.versions {
		if version < base {
			delete(s.versions, version)
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
