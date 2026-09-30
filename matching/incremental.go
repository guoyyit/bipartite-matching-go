package matching

import "math/big"

// WarmSolver keeps the previous solution and re-optimizes it after a set of
// committed changes instead of recomputing from scratch.
type WarmSolver struct {
	scope Scope
	prev  map[Pair]int // flow on each pair in the last committed matching
	used  bool
}

// NewWarmSolver builds a warm solver whose first Apply solves from scratch.
func NewWarmSolver(scope Scope) *WarmSolver {
	return &WarmSolver{scope: scope, prev: map[Pair]int{}}
}

// Apply incorporates a committed graph version and returns its solution.
func (s *WarmSolver) Apply(g *Graph) (*Solution, error) {
	if err := validInputCaps(g, s.scope); err != nil {
		return nil, err
	}
	allowed, absSum := canonicalAllowed(g)
	scale := new(big.Int).Lsh(big.NewInt(1), uint(len(allowed)))
	bonus := absSum + 1
	net := buildFlowNet(g, s.scope, allowed, scale, bonus)

	if s.scope == ScopePerfect {
		var seed map[Pair]int
		if s.used {
			seed = s.prev
		}
		if err := net.solvePerfect(g, seed); err != nil {
			s.prev = map[Pair]int{}
			return nil, err
		}
		net.breakTies(s.scope)
	} else if !s.used {
		net.optimizeSSP(s.scope)
		net.breakTies(s.scope)
	} else {
		// Close the open s-t network with a free return arc, reinstall the
		// previous matching along its complete source->left->right->sink
		// paths (so vertex caps remain enforced), and locally re-optimize.
		// Every repair (swap, cardinality gain or shedding) appears as a
		// strictly negative residual cycle against this circulation.
		sourceArc := net.terminalArcFromSource()
		sinkArc := net.terminalArcToSink()
		var totalCap int64
		for _, e := range sourceArc {
			totalCap += e.cap
		}
		circulationCap := totalCap + int64(len(net.pairArc)) + 1
		returnArc := net.addEdge(net.sink, net.source, circulationCap, big.NewInt(0), nil)
		seededCount := int64(0)
		for _, e := range net.pairArc {
			if s.prev[*e.pair] == 0 || e.cap <= 0 {
				continue
			}
			sourceEdge := sourceArc[e.from]
			sinkEdge := sinkArc[e.to]
			if sourceEdge == nil || sinkEdge == nil || sourceEdge.cap <= 0 || sinkEdge.cap <= 0 {
				continue // vertex cap would be violated: re-route instead
			}
			sendUnit(net, sourceEdge, e, sinkEdge)
			seededCount++
		}
		// Balance the seeded s-t flow on the return arc: forward T->S keeps
		// circulationCap-k capacity while k units are pre-installed so the
		// residual circulation represents exactly the prior flow value.
		returnArc.cap -= seededCount
		net.adj[returnArc.to][returnArc.rev].cap += seededCount
		net.reoptimize(s.scope)
		net.breakTiesWarm()
	}

	sol := net.extract(g, s.scope)
	s.prev = make(map[Pair]int, len(sol.Matched))
	for _, match := range sol.Matched {
		s.prev[match.Pair]++
	}
	s.used = true
	return sol, nil
}

// terminalArcFromSource maps each left vertex to its source->vertex arc.
func (net *flowNet) terminalArcFromSource() map[int]*arc {
	out := map[int]*arc{}
	for _, e := range net.adj[net.source] {
		if e.pair == nil && e.to < net.source {
			out[e.to] = e
		}
	}
	return out
}

// terminalArcToSink maps each right vertex to its vertex->sink arc.
func (net *flowNet) terminalArcToSink() map[int]*arc {
	out := map[int]*arc{}
	for v := 0; v < net.source; v++ {
		for _, e := range net.adj[v] {
			if e.to == net.sink && e.pair == nil {
				out[v] = e
			}
		}
	}
	return out
}

// sendUnit pushes one flow unit along source->left, the pair arc and
// right->sink, decrementing all forward residual capacities.
func sendUnit(net *flowNet, sourceEdge, pairEdge, sinkEdge *arc) {
	push(net, sourceEdge)
	push(net, pairEdge)
	push(net, sinkEdge)
}

func push(net *flowNet, e *arc) {
	e.cap--
	net.adj[e.to][e.rev].cap++
}

// reoptimize drives an arbitrary feasible flow of the closed circulation to
// the unique optimum. It first cancels negative residual cycles (converting
// the flow into a shortest-path-optimal circulation), then adjusts total
// cardinality through shortest source->sink paths. MaxWeight stops once the
// shortest such path is non-negative; MaxCardinality pushes while a path
// exists.
func (net *flowNet) reoptimize(scope Scope) {
	net.cancelNegativeCycles()
	for {
		// Forward source->sink paths add cardinality.
		dist, parentNode, parentArc := net.shortestPath(net.source)
		if parentNode[net.sink] != -1 {
			if scope != ScopeMaxWeight || dist[net.sink].Sign() < 0 {
				net.augment(net.sink, parentNode, parentArc)
				net.cancelNegativeCycles()
				continue
			}
		}
		// Reverse sink->source paths shed cardinality. Only MaxWeight is
		// allowed to shed. A zero-primary path is shed as well, because the
		// objective stage never keeps a contribution-less edge (canonical
		// tie preference is layered on separately, after this phase).
		if scope == ScopeMaxWeight {
			backDist, backNode, backArc := net.shortestPath(net.sink)
			if backNode[net.source] != -1 && backDist[net.source].Sign() <= 0 {
				net.augment(net.source, backNode, backArc)
				net.cancelNegativeCycles()
				continue
			}
		}
		return
	}
}
