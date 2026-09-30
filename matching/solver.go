package matching

import (
	"math/big"
	"sort"
)

// arc is one residual-network directed edge.
type arc struct {
	from int
	to   int
	rev  int
	cap  int64
	cost *big.Int
	pair *Pair // non-nil only on left -> right arcs
}

type flowNet struct {
	n          int
	adj        [][]*arc
	pairArc    []*arc // original left -> right arcs, in canonical order
	source     int
	sink       int
	perfectTie bool // perfect circulation uses the opposite tie sign
	sspFlow    int64
}

func (net *flowNet) addEdge(from, to int, cap int64, cost *big.Int, pair *Pair) *arc {
	forward := &arc{from: from, to: to, cap: cap, cost: new(big.Int).Set(cost), pair: pair}
	reverse := &arc{from: to, to: from, cap: 0, cost: new(big.Int).Neg(cost)}
	forward.rev = len(net.adj[to])
	reverse.rev = len(net.adj[from])
	net.adj[from] = append(net.adj[from], forward)
	net.adj[to] = append(net.adj[to], reverse)
	return forward
}

// addLowerEdge registers an arc with a mandatory lower bound of one in the
// reduced-capacity network used by the lower-bound circulation transform:
// the forward residual is cap-1 and the reverse residual starts empty.
// Reverse capacity opens only when extra flow is actually augmented, so a
// mandatory unit can never be canceled below its lower bound.
func (net *flowNet) addLowerEdge(from, to int, cap int64, cost *big.Int) *arc {
	forward := &arc{from: from, to: to, cap: cap - 1, cost: new(big.Int).Set(cost)}
	reverse := &arc{from: to, to: from, cap: 0, cost: new(big.Int).Neg(cost)}
	forward.rev = len(net.adj[to])
	reverse.rev = len(net.adj[from])
	net.adj[from] = append(net.adj[from], forward)
	net.adj[to] = append(net.adj[to], reverse)
	return forward
}

// shortestPath runs Bellman-Ford (correct on negative-cost residual networks)
// with a fixed arc scan order and strict-only relaxations, so equal-length
// alternatives keep the deterministic first predecessor.
func (net *flowNet) shortestPath(src int) (dist []*big.Int, parentNode []int, parentArc []*arc) {
	inf := new(big.Int).Lsh(big.NewInt(1), 128)
	dist = make([]*big.Int, net.n)
	parentNode = make([]int, net.n)
	parentArc = make([]*arc, net.n)
	for i := range dist {
		dist[i] = new(big.Int).Set(inf)
		parentNode[i] = -1
	}
	dist[src].SetInt64(0)
	for round := 0; round < net.n-1; round++ {
		changed := false
		for u := 0; u < net.n; u++ {
			if dist[u].Cmp(inf) == 0 {
				continue
			}
			for _, e := range net.adj[u] {
				if e.cap <= 0 {
					continue
				}
				cand := new(big.Int).Add(dist[u], e.cost)
				if cand.Cmp(dist[e.to]) < 0 {
					dist[e.to].Set(cand)
					parentNode[e.to] = u
					parentArc[e.to] = e
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}
	return dist, parentNode, parentArc
}

func (net *flowNet) augment(dst int, parentNode []int, parentArc []*arc) int64 {
	amount := int64(-1)
	for v := dst; parentNode[v] != -1; v = parentNode[v] {
		e := parentArc[v]
		if amount < 0 || e.cap < amount {
			amount = e.cap
		}
	}
	for v := dst; parentNode[v] != -1; v = parentNode[v] {
		e := parentArc[v]
		e.cap -= amount
		net.adj[e.to][e.rev].cap += amount
	}
	return amount
}

// findPath finds any residual path from src to dst with BFS. Feasibility is
// cost-agnostic, so negative-cost cycles cannot corrupt predecessor chains.
func (net *flowNet) findPath(src, dst int) (parentNode []int, parentArc []*arc) {
	parentNode = make([]int, net.n)
	parentArc = make([]*arc, net.n)
	for i := range parentNode {
		parentNode[i] = -1
	}
	queue := []int{src}
	parentNode[src] = -1
	found := false
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, e := range net.adj[u] {
			if !found && e.to != src && e.cap > 0 && parentNode[e.to] == -1 {
				parentNode[e.to] = u
				parentArc[e.to] = e
				if e.to == dst {
					found = true
				}
				queue = append(queue, e.to)
			}
		}
	}
	return parentNode, parentArc
}

func vertexCap(caps map[string]int, vertex string) int {
	if value, ok := caps[vertex]; ok {
		return value
	}
	return 1
}

// Solve computes a deterministic matching for a complete graph snapshot.
//
// Costs are integer-scaled with two strict-priority perturbation layers so the
// optimum is a unique, reproducible edge set: primary objective, then a binary
// code over edges in canonical order that prefers including the earliest pair.
func Solve(g *Graph, scope Scope) (*Solution, error) {
	if g == nil {
		return nil, errInvalidGraph("nil graph")
	}
	if scope < ScopeMaxWeight || scope > ScopePerfect {
		return nil, errInvalidGraph("unknown solve scope")
	}

	if err := validInputCaps(g, scope); err != nil {
		return nil, err
	}

	allowed, absSum := canonicalAllowed(g)
	// scale = 2^E dominates the binary tie code, whose bits are 2^(E-1-idx).
	scale := new(big.Int).Lsh(big.NewInt(1), uint(len(allowed)))
	bonus := absSum + 1 // cardinality bonus, dominates every total weight swing

	net := buildFlowNet(g, scope, allowed, scale, bonus)

	if scope == ScopePerfect {
		if err := net.solvePerfect(g, nil); err != nil {
			return nil, err
		}
	} else {
		net.optimizeSSP(scope)
	}
	net.breakTies(scope)

	return net.extract(g, scope), nil
}

// buildFlowNet constructs the residual network for a scope, reusing the
// canonical edge order and perturbation scale supplied by the caller.
func buildFlowNet(g *Graph, scope Scope, allowed []Pair, scale *big.Int, bonus int64) *flowNet {
	indexOfLeft := make(map[string]int, len(g.Left))
	indexOfRight := make(map[string]int, len(g.Right))
	for i, vertex := range g.Left {
		indexOfLeft[vertex] = i
	}
	for i, vertex := range g.Right {
		indexOfRight[vertex] = i
	}
	base := len(g.Left) + len(g.Right)
	net := &flowNet{
		n:          base + 2,
		adj:        make([][]*arc, base+2),
		source:     base,
		sink:       base + 1,
		perfectTie: scope != ScopeMaxWeight,
	}
	zero := big.NewInt(0)

	for i, vertex := range g.Left {
		capValue := vertexCap(g.LeftCap, vertex)
		if scope == ScopePerfect {
			// Pre-install one mandatory unit; the forward arc keeps the
			// residual capacity and the reverse arc already holds the unit.
			net.addLowerEdge(net.source, i, int64(capValue), zero)
		} else {
			net.addEdge(net.source, i, int64(capValue), zero, nil)
		}
	}
	for j, vertex := range g.Right {
		capValue := vertexCap(g.RightCap, vertex)
		if scope == ScopePerfect {
			net.addLowerEdge(len(g.Left)+j, net.sink, int64(capValue), zero)
		} else {
			net.addEdge(len(g.Left)+j, net.sink, int64(capValue), zero, nil)
		}
	}
	for _, pair := range allowed {
		weight := g.Edges[pair]
		primary := new(big.Int).SetInt64(weight)
		if scope == ScopeMaxCardinality {
			primary.Add(primary, big.NewInt(bonus))
		}
		// Objective-stage cost (minimize): -scale*primary. The tie bit is
		// applied in a dedicated post-pass over equal-objective optima.
		cost := new(big.Int).Mul(scale, primary)
		cost.Neg(cost)
		pairArc := net.addEdge(indexOfLeft[pair.Left], len(g.Left)+indexOfRight[pair.Right], 1, cost, &pair)
		net.pairArc = append(net.pairArc, pairArc)
	}

	return net
}

// optimizeSSP repeatedly augments along the shortest source-sink residual
// path. MaxWeight stops at the first non-negative path; MaxCardinality keeps
// augmenting while any path exists.
func (net *flowNet) optimizeSSP(scope Scope) {
	for {
		dist, parentNode, parentArc := net.shortestPath(net.source)
		if parentNode[net.sink] == -1 {
			return
		}
		if scope == ScopeMaxWeight && dist[net.sink].Sign() >= 0 {
			return
		}
		net.sspFlow += net.augment(net.sink, parentNode, parentArc)
	}
}

// breakTies selects the canonical edge set among all optima that share the
// primary objective. Pair arcs are retagged with an infinitesimal additive
// tie code (a bit that prefers including the earliest canonical pair); the
// small magnitude guarantees a tie-improving swap can never change the
// primary value. Cycles are then cancelled on the closed network.
func (net *flowNet) breakTies(scope Scope) {
	net.retagTieCosts()
	if scope != ScopePerfect {
		// Close the s-t network with a return arc pre-loaded with the flow
		// already sent, so cardinality-preserving reroutes through source
		// and sink are expressible in both residual directions.
		capacity := net.sspFlow + int64(net.n) + int64(len(net.pairArc)) + 1
		returnArc := net.addEdge(net.sink, net.source, capacity, big.NewInt(0), nil)
		returnArc.cap -= net.sspFlow
		net.adj[net.source][returnArc.rev].cap += net.sspFlow
	}
	net.cancelNegativeCycles()
}

// breakTiesWarm retags pair costs on a network that is already closed and
// primary-optimal, then cancels tie-improving cycles. The sink->source return
// arc stays open so a tie-improving swap can reroute through source/sink;
// residual cycles preserve net flow, and the infinitesimal tie magnitude
// guarantees the primary objective cannot change.
func (net *flowNet) breakTiesWarm() {
	net.retagTieCosts()
	net.cancelNegativeCycles()
}

func (net *flowNet) retagTieCosts() {
	count := len(net.pairArc)
	one := big.NewInt(1)
	for idx, e := range net.pairArc {
		bit := new(big.Int).Lsh(one, uint(count-1-idx))
		if net.perfectTie {
			// Cardinality-fixed problems (perfect, max-cardinality) include
			// the earliest canonical pair at equal objective.
			e.cost.Sub(e.cost, bit)
			net.adj[e.to][e.rev].cost.Add(net.adj[e.to][e.rev].cost, bit)
		} else {
			// MaxWeight sheds zero/equal contributions and only includes a
			// pair when it strictly helps; its tie sign is the opposite.
			e.cost.Add(e.cost, bit)
			net.adj[e.to][e.rev].cost.Sub(net.adj[e.to][e.rev].cost, bit)
		}
	}
}

// validInputCaps reports structural problems that make a solve invalid.
func validInputCaps(g *Graph, scope Scope) error {
	for _, vertex := range g.Left {
		if vertexCap(g.LeftCap, vertex) < 0 {
			return errInvalidGraph("negative match cap for vertex " + vertex)
		}
	}
	for _, vertex := range g.Right {
		if vertexCap(g.RightCap, vertex) < 0 {
			return errInvalidGraph("negative match cap for vertex " + vertex)
		}
	}
	if scope == ScopePerfect {
		for _, vertex := range g.Left {
			if vertexCap(g.LeftCap, vertex) <= 0 {
				return &InfeasibleError{Code: FailureCapInfeasible, Message: "vertex " + vertex + " cannot be matched"}
			}
		}
		for _, vertex := range g.Right {
			if vertexCap(g.RightCap, vertex) <= 0 {
				return &InfeasibleError{Code: FailureCapInfeasible, Message: "vertex " + vertex + " cannot be matched"}
			}
		}
	}
	return nil
}

// canonicalAllowed lists forbidden-filtered edges in stable pair order and
// returns the sum of absolute weights used for the cardinality bonus.
func canonicalAllowed(g *Graph) ([]Pair, int64) {
	allowed := make([]Pair, 0, len(g.Edges))
	var absSum int64
	for pair, weight := range g.Edges {
		if _, banned := g.Forbidden[pair]; banned {
			continue
		}
		allowed = append(allowed, pair)
		if weight >= 0 {
			absSum += weight
		} else {
			absSum += -weight
		}
	}
	sort.Slice(allowed, func(i, j int) bool {
		if allowed[i].Left != allowed[j].Left {
			return allowed[i].Left < allowed[j].Left
		}
		return allowed[i].Right < allowed[j].Right
	})
	return allowed, absSum
}

// seedPerfectResidual pushes the prior matching onto the already-feasible
// residual circulation wherever the full source->left->right->sink path still
// has capacity; everything else is left for cycle re-optimization.
func (net *flowNet) seedPerfectResidual(g *Graph, prev map[Pair]int,
	sourceArc map[int]*arc, sinkArc map[int]*arc) {
	for _, e := range net.pairArc {
		if prev[*e.pair] == 0 || e.cap <= 0 {
			continue
		}
		sourceEdge := sourceArc[e.from]
		sinkEdge := sinkArc[e.to]
		if sourceEdge == nil || sinkEdge == nil || sourceEdge.cap <= 0 || sinkEdge.cap <= 0 {
			continue
		}
		sendUnit(net, sourceEdge, e, sinkEdge)
	}
}

// solvePerfect enforces a lower bound of one on every source/left and
// right/sink arc via the standard super-source/super-sink circulation
// transform and proves feasibility by saturating the required flow. When prev
// is non-nil, the prior matching is seeded so only changed vertices require
// re-balancing.
func (net *flowNet) solvePerfect(g *Graph, prev map[Pair]int) error {
	superSource := net.n
	superSink := net.n + 1
	net.n += 2
	net.adj = append(net.adj, nil, nil)

	requiredFlow := int64(len(g.Left))
	if int64(len(g.Right)) > requiredFlow {
		requiredFlow = int64(len(g.Right))
	}
	// The sink->source return arc must carry exactly requiredFlow units;
	// pre-installing that lower bound forbids any spurious extra circulation.
	net.addLowerEdge(net.sink, net.source, requiredFlow, big.NewInt(0))

	// imbalance = incoming pre-installed flow - outgoing pre-installed flow.
	balance := make([]int, net.n+2)
	for i := range g.Left {
		balance[i]++
		balance[net.source]--
	}
	for j := range g.Right {
		balance[net.sink]++
		balance[len(g.Left)+j]--
	}
	balance[net.source] += int(requiredFlow)
	balance[net.sink] -= int(requiredFlow)

	var required int64
	for v, b := range balance {
		switch {
		case b > 0:
			net.addEdge(superSource, v, int64(b), big.NewInt(0), nil)
			required += int64(b)
		case b < 0:
			net.addEdge(v, superSink, int64(-b), big.NewInt(0), nil)
		}
	}

	var sent int64
	for sent < required {
		parentNode, parentArc := net.findPath(superSource, superSink)
		if parentNode[superSink] == -1 {
			return &InfeasibleError{Code: FailurePerfectUnreachable, Message: "no perfect matching satisfies the constraints"}
		}
		sent += net.augment(superSink, parentNode, parentArc)
	}

	// The mandatory flow is now balanced. Remove super artifacts from the
	// residual network before cycle cancellation so they can never alter
	// the real circulation.
	net.adj[superSource] = nil
	net.adj[superSink] = nil

	if prev != nil {
		// Feasibility above is seed-independent. Warm-start optimization by
		// reinstalling prior pair flow that still fits the residual network;
		// units that no longer fit are simply re-optimized from scratch.
		sourceArc := net.terminalArcFromSource()
		sinkArc := net.terminalArcToSink()
		net.seedPerfectResidual(g, prev, sourceArc, sinkArc)
	}

	// Optimize weight and resolve ties on the closed circulation by
	// cancelling every strictly negative perturbed-cost cycle.
	net.cancelNegativeCycles()
	return nil
}

// cancelNegativeCycles repeatedly finds a residual cycle of strictly negative
// perturbed cost with Bellman-Ford and augments around it. A zero-distance
// start at every node stands in for a virtual all-connected source.

func (net *flowNet) cancelNegativeCycles() {
	for {
		dist := make([]*big.Int, net.n)
		// pathArc[v] is the complete predecessor-edge chain to v, cloned at
		// the moment v was last relaxed. This keeps every stored chain a
		// single-round-consistent walk, so a late relaxation exposes a real
		// cycle rather than a mix of edges from different rounds.
		pathArc := make([][]*arc, net.n)
		for i := range dist {
			dist[i] = big.NewInt(0)
		}
		var found []*arc
		for round := 0; round < net.n && found == nil; round++ {
			for u := 0; u < net.n; u++ {
				for _, e := range net.adj[u] {
					if e.cap <= 0 {
						continue
					}
					cand := new(big.Int).Add(dist[u], e.cost)
					if cand.Cmp(dist[e.to]) < 0 {
						dist[e.to].Set(cand)
						chain := make([]*arc, 0, len(pathArc[u])+1)
						chain = append(chain, pathArc[u]...)
						chain = append(chain, e)
						if len(chain) > net.n {
							// A simple path has at most n-1 edges; more
							// means the chain has wrapped onto a cycle.
							found = chain
							break
						}
						pathArc[e.to] = chain
					}
				}
				if found != nil {
					break
				}
			}
		}
		if found == nil {
			return
		}
		// found is a walk v0->v1->...; find the first repeated vertex and
		// keep exactly the arcs between its two occurrences.
		firstSeen := map[int]int{found[0].from: 0}
		cycle := []*arc(nil)
		for i, e := range found {
			if start, repeated := firstSeen[e.to]; repeated {
				cycle = found[start : i+1]
				break
			}
			firstSeen[e.to] = i + 1
		}
		if len(cycle) == 0 {
			return
		}
		found = cycle
		amount := int64(-1)
		for _, e := range found {
			if amount < 0 || e.cap < amount {
				amount = e.cap
			}
		}
		for _, e := range found {
			e.cap -= amount
			net.adj[e.to][e.rev].cap += amount
		}
	}
}

func (net *flowNet) extract(g *Graph, scope Scope) *Solution {
	sol := &Solution{Scope: scope, Feasible: true}
	matchedLeft := make(map[string]int, len(g.Left))
	matchedRight := make(map[string]int, len(g.Right))
	for _, e := range net.pairArc {
		if e.cap == 0 {
			pair := *e.pair
			sol.Matched = append(sol.Matched, Match{Pair: pair, Weight: g.Edges[pair]})
			sol.Weight += g.Edges[pair]
			sol.Cardinality++
			matchedLeft[pair.Left]++
			matchedRight[pair.Right]++
		}
	}
	sort.Slice(sol.Matched, func(i, j int) bool {
		if sol.Matched[i].Left != sol.Matched[j].Left {
			return sol.Matched[i].Left < sol.Matched[j].Left
		}
		return sol.Matched[i].Right < sol.Matched[j].Right
	})
	for _, vertex := range g.Left {
		if matchedLeft[vertex] == 0 {
			sol.UnmatchedLeft = append(sol.UnmatchedLeft, vertex)
		}
	}
	for _, vertex := range g.Right {
		if matchedRight[vertex] == 0 {
			sol.UnmatchedRight = append(sol.UnmatchedRight, vertex)
		}
	}
	sort.Strings(sol.UnmatchedLeft)
	sort.Strings(sol.UnmatchedRight)
	return sol
}

type validationError string

func (e validationError) Error() string { return string(e) }

func errInvalidGraph(message string) error { return validationError(message) }
