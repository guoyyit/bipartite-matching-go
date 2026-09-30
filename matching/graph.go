package matching

// Scope selects which matching problem a solve should target.
type Scope int

const (
	// ScopeMaxWeight maximizes total edge weight; vertices may stay unmatched.
	ScopeMaxWeight Scope = iota
	// ScopeMaxCardinality maximizes the number of matched edges first.
	ScopeMaxCardinality
	// ScopePerfect requires every vertex on both sides to be matched.
	ScopePerfect
)

// String returns the canonical name of a scope.
func (s Scope) String() string {
	switch s {
	case ScopeMaxWeight:
		return "max_weight"
	case ScopeMaxCardinality:
		return "max_cardinality"
	case ScopePerfect:
		return "perfect"
	default:
		return "unknown"
	}
}

// Pair identifies a (left, right) vertex pair.
type Pair struct {
	Left  string
	Right string
}

// Edge is a weighted pair.
type Edge struct {
	Pair
	Weight int64
}

// Graph is one complete input state: two vertex groups, weighted edges and
// matching constraints (forbidden pairs, per-vertex match-count caps).
type Graph struct {
	Left      []string
	Right     []string
	Edges     map[Pair]int64
	Forbidden map[Pair]struct{}
	LeftCap   map[string]int
	RightCap  map[string]int
}

// NewGraph returns an empty graph.
func NewGraph() *Graph {
	return &Graph{
		Edges:     map[Pair]int64{},
		Forbidden: map[Pair]struct{}{},
		LeftCap:   map[string]int{},
		RightCap:  map[string]int{},
	}
}

// Clone returns a deep copy of the graph.
func (g *Graph) Clone() *Graph {
	if g == nil {
		return nil
	}
	cp := &Graph{
		Left:      append([]string(nil), g.Left...),
		Right:     append([]string(nil), g.Right...),
		Edges:     make(map[Pair]int64, len(g.Edges)),
		Forbidden: make(map[Pair]struct{}, len(g.Forbidden)),
		LeftCap:   make(map[string]int, len(g.LeftCap)),
		RightCap:  make(map[string]int, len(g.RightCap)),
	}
	for pair, weight := range g.Edges {
		cp.Edges[pair] = weight
	}
	for pair := range g.Forbidden {
		cp.Forbidden[pair] = struct{}{}
	}
	for vertex, capValue := range g.LeftCap {
		cp.LeftCap[vertex] = capValue
	}
	for vertex, capValue := range g.RightCap {
		cp.RightCap[vertex] = capValue
	}
	return cp
}

// ChangeKind enumerates committed graph mutations.
type ChangeKind int

const (
	ChangeAddEdge ChangeKind = iota
	ChangeDeleteEdge
	ChangeSetWeight
	ChangeAddLeft
	ChangeAddRight
	ChangeSetLeftCap
	ChangeSetRightCap
	ChangeForbid
	ChangeAllow
)

// Change is one atomic graph mutation inside a transaction.
type Change struct {
	Kind   ChangeKind
	Pair   Pair
	Weight int64
	Cap    int
}

// Match is one selected edge in a solved matching.
type Match struct {
	Pair
	Weight int64
}

// FailureCode identifies why a constrained solve is infeasible.
type FailureCode string

const (
	FailurePerfectUnreachable FailureCode = "perfect_unreachable"
	FailureCapInfeasible      FailureCode = "cap_infeasible"
)

// InfeasibleError reports a constraint-induced failure with a stable code.
type InfeasibleError struct {
	Code    FailureCode
	Message string
}

func (e *InfeasibleError) Error() string {
	if e.Message != "" {
		return string(e.Code) + ": " + e.Message
	}
	return string(e.Code)
}

// Solution is the deterministic result of one solve.
type Solution struct {
	Scope          Scope
	Weight         int64
	Cardinality    int
	Matched        []Match
	UnmatchedLeft  []string
	UnmatchedRight []string
	Feasible       bool
}
