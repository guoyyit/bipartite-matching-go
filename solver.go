package matching

import (
	"math"
	"sort"
)

// 确定性平局消解规则：
//  1. 主目标由 Mode 决定——完美匹配优先可行、最大基数优先匹配对数、
//     最大权只看总权重；
//  2. 在主目标（基数与总权重）相同的全部最优解中，选择按边键 (U,V)
//     字典序列出的匹配边序列最小的解：逐条边按字典序优先强制包含，
//     包含后仍能达到同一最优值才保留，否则永久排除。
//
// 因此同一输入（图、模式、约束）必然得到一致结果。

const flowInf = math.MaxInt64 / 4

// fedge 是费用流网络中的一条有向残量弧。
type fedge struct {
	to   int
	rev  int
	cap  int
	cost int64
	eid  int // L->R 正向弧对应的图边下标，否则为 -1
}

type fnet struct {
	n int
	g [][]fedge
}

func newFnet(n int) *fnet {
	return &fnet{n: n, g: make([][]fedge, n)}
}

// addEdge 添加容量 cap、费用 cost 的有向边及其反向残量弧，
// 返回正向弧在 g[fr] 中的下标。
func (fn *fnet) addEdge(fr, to, cap int, cost int64, eid int) int {
	idx := len(fn.g[fr])
	fn.g[fr] = append(fn.g[fr], fedge{to: to, rev: len(fn.g[to]), cap: cap, cost: cost, eid: eid})
	fn.g[to] = append(fn.g[to], fedge{to: fr, rev: idx, cap: 0, cost: -cost, eid: -1})
	return idx
}

// flowArc 记录一条 L->R 正向弧的位置，用于提取匹配边。
type flowArc struct {
	node int
	idx  int
	eid  int
	lb   int
}

// solveSpec 描述一次求解的全部固定参数，供冷启动与逐边平局消解复用。
type solveSpec struct {
	mode    Mode
	lefts   []string
	rights  []string
	edges   []Edge // g.Edges()，下标即字典序
	forbid  map[[2]string]struct{}
	capL    map[string]int
	capR    map[string]int
	exclude map[int]struct{} // 强制不使用的边下标
	include map[int]struct{} // 强制使用（下界 1）的边下标
	perfect bool
}

// Solve 在给定图上按指定求解范围与约束求解决定性的最大权匹配。
// 约束使求解不可行时返回 *SolveError，且不返回任何部分结果。
func Solve(g *Graph, mode Mode, cons *Constraints) (*Result, error) {
	sp, err := buildSpec(g, mode, cons)
	if err != nil {
		return nil, err
	}
	used, err := sp.run()
	if err != nil {
		return nil, err
	}
	used = sp.lexResolve(used)
	return sp.toResult(g.Version(), used), nil
}

// resolveWithWarm 以一份已知最优匹配（基数与总权重均最优）为锚，执行统一的
// 确定性平局消解并返回结果。增量求解器提供 warmUsed，保证与冷启动同一答案。
func resolveWithWarm(g *Graph, mode Mode, cons *Constraints, warmUsed map[[2]string]struct{}) (*Result, error) {
	sp, err := buildSpec(g, mode, cons)
	if err != nil {
		return nil, err
	}
	anchor := map[int]struct{}{}
	for i, e := range sp.edges {
		if _, ok := warmUsed[[2]string{e.U, e.V}]; ok {
			anchor[i] = struct{}{}
		}
	}
	used := sp.lexResolve(anchor)
	return sp.toResult(g.Version(), used), nil
}

// buildSpec 校验图与约束。约束非法时返回 FailureInvalidConstraint，绝不静默放宽；
// 被禁止的边直接不建弧，而不是事后裁剪结果。
func buildSpec(g *Graph, mode Mode, cons *Constraints) (*solveSpec, error) {
	if mode < ModePerfect || mode > ModeMaxWeight {
		return nil, &SolveError{Reason: FailureInvalidGraph, Message: "unknown solve mode"}
	}
	sp := &solveSpec{
		mode:    mode,
		lefts:   g.LeftVertices(),
		rights:  g.RightVertices(),
		edges:   g.Edges(),
		forbid:  map[[2]string]struct{}{},
		capL:    map[string]int{},
		capR:    map[string]int{},
		exclude: map[int]struct{}{},
		include: map[int]struct{}{},
		perfect: mode == ModePerfect,
	}
	if cons != nil {
		for k := range cons.Forbidden {
			if k[0] == "" || k[1] == "" {
				return nil, &SolveError{Reason: FailureInvalidConstraint, Message: "forbidden pair has empty endpoint"}
			}
			sp.forbid[k] = struct{}{}
		}
		for v, c := range cons.MaxLeft {
			if v == "" || c <= 0 {
				return nil, &SolveError{Reason: FailureInvalidConstraint, Message: "left vertex cap must be >= 1: " + v}
			}
			sp.capL[v] = c
		}
		for v, c := range cons.MaxRight {
			if v == "" || c <= 0 {
				return nil, &SolveError{Reason: FailureInvalidConstraint, Message: "right vertex cap must be >= 1: " + v}
			}
			sp.capR[v] = c
		}
	}
	for _, e := range sp.edges {
		if e.Weight == math.MinInt64 {
			return nil, &SolveError{Reason: FailureInvalidGraph, Message: "weight out of int64 range on edge " + e.U + "->" + e.V}
		}
	}
	return sp, nil
}

// capOf 返回某侧顶点的配对数上限，缺省为 1。
func capOf(caps map[string]int, v string) int {
	if c, ok := caps[v]; ok {
		return c
	}
	return 1
}

// run 构造最小费用环流并求最优，返回匹配边下标集合。
func (sp *solveSpec) run() (map[int]struct{}, error) {
	// 构造含下界的网络，得到一个可行的初始 s->t 流（不满足下界则直接失败，
	// 不返回任何部分结果），随后在其残量网络上做最小费用流优化。
	fn, arcs, s, t, feasible := sp.buildNetwork()
	if !feasible {
		return nil, &SolveError{
			Reason:  FailurePerfectInfeasible,
			Message: "perfect match infeasible under current graph and constraints",
		}
	}

	// 在保持当前流量不变的前提下取消负费用环，得到该流量下的最小费用流；
	// 这样后续逐次最短路增广每一步都保持最优。
	fn.cancelNegCyclesActive(t + 1)

	// 逐次最短路增广：
	//   - 完美匹配：下界已强制每个顶点恰好一单位，流量基数唯一，禁止再
	//     增广（否则会利用 cap>1 制造额外配对）；仅靠上面的负环消解优化权重；
	//   - 最大基数：增广到不存在 s->t 路（最小费用最大流）；
	//   - 最大权：仅在最短路严格为负（提高总权）时增广。
	if sp.mode == ModeMaxCardinality {
		fn.augmentSSP(s, t, true)
	} else if sp.mode == ModeMaxWeight {
		fn.augmentSSP(s, t, false)
	}

	used := make(map[int]struct{}, len(arcs))
	for _, a := range arcs {
		if fn.g[a.node][a.idx].cap == 0 {
			used[a.eid] = struct{}{}
		}
	}
	return used, nil
}

// arcSpec 描述待构建的一条概念弧及其下界。
type arcSpec struct {
	fr, to, lb, upper int
	cost              int64
	eid               int
}

// buildNetwork 构造含下界的 s->t 网络，并用标准“超源/超汇 + t->s 回流边”
// 变换求一个可行流。若下界不可满足，feasible 为假（此时返回的网络不可用）。
// 成功时，残量网络中保留的流量即为一个合法 s->t 流：可行性最大流沿
// t->s 环流推动的每一单位都对应真实 s->t 路径，配合各弧下界残量容量，
// L->R 正向弧 cap==0 即表示该匹配边被使用。
func (sp *solveSpec) buildNetwork() (*fnet, []flowArc, int, int, bool) {
	s := 0
	t := 1 + len(sp.lefts) + len(sp.rights)
	fn := newFnet(t + 1)
	li := make(map[string]int, len(sp.lefts))
	ri := make(map[string]int, len(sp.rights))
	for i, v := range sp.lefts {
		li[v] = 1 + i
	}
	for i, v := range sp.rights {
		ri[v] = 1 + len(sp.lefts) + i
	}

	specs := make([]arcSpec, 0, len(sp.edges)+len(sp.lefts)+len(sp.rights))
	add := func(fr, to, lb, upper int, cost int64, eid int) {
		specs = append(specs, arcSpec{fr: fr, to: to, lb: lb, upper: upper, cost: cost, eid: eid})
	}
	for _, v := range sp.lefts {
		lb := 0
		if sp.perfect {
			lb = 1
		}
		upper := capOf(sp.capL, v)
		if sp.perfect {
			upper = 1 // 完美匹配要求每个顶点恰好出现一次，覆盖配对数上限
		}
		add(s, li[v], lb, upper, 0, -1)
	}
	for eid, e := range sp.edges {
		if _, bad := sp.forbid[[2]string{e.U, e.V}]; bad {
			continue
		}
		if _, off := sp.exclude[eid]; off {
			continue
		}
		lb := 0
		if _, on := sp.include[eid]; on {
			lb = 1
		}
		add(li[e.U], ri[e.V], lb, 1, -e.Weight, eid)
	}
	for _, v := range sp.rights {
		lb := 0
		if sp.perfect {
			lb = 1
		}
		upper := capOf(sp.capR, v)
		if sp.perfect {
			upper = 1
		}
		add(ri[v], t, lb, upper, 0, -1)
	}

	balance := make([]int, fn.n)
	for _, a := range specs {
		balance[a.fr] -= a.lb
		balance[a.to] += a.lb
	}

	// 先加入所有概念弧（容量 upper-lb），记录 L->R 弧用于提取匹配。
	arcs := make([]flowArc, 0, len(sp.edges))
	for _, a := range specs {
		idx := fn.addEdge(a.fr, a.to, a.upper-a.lb, a.cost, a.eid)
		if a.eid >= 0 {
			arcs = append(arcs, flowArc{node: a.fr, idx: idx, eid: a.eid, lb: a.lb})
		}
	}

	// t->s 回流边把 s->t 流变成环流；超源/超汇驱动下界需求。
	retIdx := fn.addEdge(t, s, flowInf, 0, -1)
	retRev := fn.g[t][retIdx].rev
	ss := fn.n
	tt := fn.n + 1
	fn.n += 2
	fn.g = append(fn.g, nil, nil)
	need := 0
	for v, b := range balance {
		switch {
		case b > 0:
			fn.addEdge(ss, v, b, 0, -1)
			need += b
		case b < 0:
			fn.addEdge(v, tt, -b, 0, -1)
		}
	}
	pushed := fn.pushBFS(ss, tt)
	if pushed < need {
		return nil, nil, s, t, false
	}

	// 可行流已得到。断开超源/超汇，并把 t->s 回流边的当前流量“固化”为
	// 显式 s->t 流：禁用回流边本身。由于可行性最大流推动的每单位都走过
	// 真实 s->...->t 路径，禁用后这些路径上的残量容量就是匹配状态。
	fn.disableNode(ss)
	fn.disableNode(tt)
	fn.g[t][retIdx].cap = 0
	fn.g[s][retRev].cap = 0
	return fn, arcs, s, t, true
}

// disableNode 禁用与超级源/汇相连的全部弧（含反向弧）。
func (fn *fnet) disableNode(v int) {
	for _, e := range fn.g[v] {
		fn.g[e.to][e.rev].cap = 0
	}
	fn.g[v] = nil
}

// pushBFS 无视费用，用 BFS 反复找增广路并推满流量，返回总流量。
// 仅用于下界可行性检查（此时费用不影响可行性）。
func (fn *fnet) pushBFS(s, t int) int {
	total := 0
	for {
		pvV := make([]int, fn.n)
		pvE := make([]int, fn.n)
		for i := range pvV {
			pvV[i] = -1
		}
		pvV[s] = s
		q := []int{s}
		for len(q) > 0 && pvV[t] < 0 {
			u := q[0]
			q = q[1:]
			for ei, e := range fn.g[u] {
				if e.cap > 0 && pvV[e.to] < 0 {
					pvV[e.to] = u
					pvE[e.to] = ei
					q = append(q, e.to)
					if e.to == t {
						break
					}
				}
			}
		}
		if pvV[t] < 0 {
			return total
		}
		bot := flowInf
		for v := t; v != s; v = pvV[v] {
			if c := fn.g[pvV[v]][pvE[v]].cap; c < bot {
				bot = c
			}
		}
		for v := t; v != s; v = pvV[v] {
			e := &fn.g[pvV[v]][pvE[v]]
			e.cap -= bot
			fn.g[v][e.rev].cap += bot
		}
		total += bot
	}
}

// cancelNegCyclesActive 在仅考虑编号 < limit 的活跃节点前提下，反复检测
// 并取消负费用环（Bellman-Ford），直到不存在负环。用于把下界可行流优化为
// 同流量下的最小费用流。节点与邻接顺序固定，结果确定。
func (fn *fnet) cancelNegCyclesActive(limit int) {
	for {
		if !fn.cancelOneNegCycleActive(limit) {
			return
		}
	}
}

// cancelOneNegCycleActive 用一轮 Bellman-Ford 检测一个负环并取消它，
// 返回是否真的取消了一个环。
func (fn *fnet) cancelOneNegCycleActive(limit int) bool {
	dist := make([]int64, fn.n)
	pvV := make([]int, fn.n)
	pvE := make([]int, fn.n)
	for i := range pvV {
		pvV[i] = -1
	}
	var relaxed int = -1
	for round := 0; round < limit; round++ {
		relaxed = -1
		for u := 0; u < limit; u++ {
			if dist[u] == flowInf {
				continue
			}
			for ei, e := range fn.g[u] {
				if e.cap <= 0 || e.to >= limit {
					continue
				}
				if nd := dist[u] + e.cost; nd < dist[e.to] {
					dist[e.to] = nd
					pvV[e.to] = u
					pvE[e.to] = ei
					relaxed = e.to
				}
			}
		}
		if relaxed < 0 {
			return false
		}
	}
	seen := make([]bool, fn.n)
	v := relaxed
	for !seen[v] {
		seen[v] = true
		v = pvV[v]
	}
	bot := flowInf
	for u := v; ; u = pvV[u] {
		if c := fn.g[pvV[u]][pvE[u]].cap; c < bot {
			bot = c
		}
		if pvV[u] == v {
			break
		}
	}
	for u := v; ; u = pvV[u] {
		e := &fn.g[pvV[u]][pvE[u]]
		e.cap -= bot
		fn.g[u][e.rev].cap += bot
		if pvV[u] == v {
			break
		}
	}
	return true
}

// augmentSSP 用逐次最短增广路求最小费用 s-t 流。
// 每次用 Bellman-Ford 在残量网络上求最短路（可处理初始负费用边）；
// allowAll 为真时增广到无路可走（最大基数/完美），否则仅在最短路严格
// 为负时增广（最大权）。邻接与节点顺序固定、严格小于松弛，结果确定。
func (fn *fnet) augmentSSP(s, t int, allowAll bool) {
	for {
		if !fn.augmentOneSSP(s, t, allowAll) {
			return
		}
	}
}

// augmentOneSSP 做一次最短路增广，返回是否成功推动了流量。
func (fn *fnet) augmentOneSSP(s, t int, allowAll bool) bool {
	dist := make([]int64, fn.n)
	pvV := make([]int, fn.n)
	pvE := make([]int, fn.n)
	inQ := make([]bool, fn.n)
	for i := range dist {
		dist[i] = flowInf
		pvV[i] = -1
	}
	dist[s] = 0
	// 超源/汇已被禁用（g 置 nil），节点仍在但不会参与。
	// 下界可行流保证残量网络无负环，SPFA 必然收敛。
	q := []int{s}
	inQ[s] = true
	for len(q) > 0 {
		u := q[0]
		q = q[1:]
		inQ[u] = false
		for ei, e := range fn.g[u] {
			if e.cap <= 0 {
				continue
			}
			if nd := dist[u] + e.cost; nd < dist[e.to] {
				dist[e.to] = nd
				pvV[e.to] = u
				pvE[e.to] = ei
				if !inQ[e.to] {
					q = append(q, e.to)
					inQ[e.to] = true
				}
			}
		}
	}
	if pvV[t] < 0 {
		return false
	}
	if !allowAll && dist[t] >= 0 {
		return false
	}
	bot := flowInf
	for v := t; v != s; v = pvV[v] {
		if c := fn.g[pvV[v]][pvE[v]].cap; c < bot {
			bot = c
		}
	}
	for v := t; v != s; v = pvV[v] {
		e := &fn.g[pvV[v]][pvE[v]]
		e.cap -= bot
		fn.g[v][e.rev].cap += bot
	}
	return true
}

// cancelInternalNegCycle 在排除 s、t 的内部节点子图上检测并取消一个
// 负费用环，返回是否取消过。环不经过 s/t，取消它只重路由已有匹配、
// 不改变 s-t 流量基数，因此可安全用于增量更新后的同基数权重再优化。
func (fn *fnet) cancelInternalNegCycle(s, t int) bool {
	dist := make([]int64, fn.n)
	pvV := make([]int, fn.n)
	pvE := make([]int, fn.n)
	for i := range pvV {
		pvV[i] = -1
	}
	internal := func(u int) bool { return u != s && u != t }
	var relaxed = -1
	for round := 0; round < fn.n; round++ {
		relaxed = -1
		for u := 0; u < fn.n; u++ {
			if !internal(u) {
				continue
			}
			for ei, e := range fn.g[u] {
				if e.cap <= 0 || !internal(e.to) {
					continue
				}
				if nd := dist[u] + e.cost; nd < dist[e.to] {
					dist[e.to] = nd
					pvV[e.to] = u
					pvE[e.to] = ei
					relaxed = e.to
				}
			}
		}
		if relaxed < 0 {
			return false
		}
	}
	seen := make([]bool, fn.n)
	v := relaxed
	for !seen[v] {
		seen[v] = true
		v = pvV[v]
	}
	bot := flowInf
	for u := v; ; u = pvV[u] {
		if c := fn.g[pvV[u]][pvE[u]].cap; c < bot {
			bot = c
		}
		if pvV[u] == v {
			break
		}
	}
	for u := v; ; u = pvV[u] {
		e := &fn.g[pvV[u]][pvE[u]]
		e.cap -= bot
		fn.g[u][e.rev].cap += bot
		if pvV[u] == v {
			break
		}
	}
	return true
}

// augmentSSPBF 与 augmentSSP 功能相同，但最短路使用有界 Bellman-Ford
// （固定 n-1 轮），即便残量网络存在负费用边也不会发散，适合增量更新后
// 可能带负边的残量网络。每次 BF 得到一致的前驱树后沿最短路推一单位。
func (fn *fnet) augmentSSPBF(s, t int, allowAll bool) {
	for {
		dist := make([]int64, fn.n)
		pvV := make([]int, fn.n)
		pvE := make([]int, fn.n)
		for i := range dist {
			dist[i] = flowInf
			pvV[i] = -1
		}
		dist[s] = 0
		// 用两缓冲的 Bellman-Ford，避免原地松弛导致前驱树不一致。
		nd := append([]int64(nil), dist...)
		for round := 0; round < fn.n-1; round++ {
			copy(nd, dist)
			changed := false
			for u := 0; u < fn.n; u++ {
				if dist[u] == flowInf {
					continue
				}
				for ei, e := range fn.g[u] {
					if e.cap <= 0 {
						continue
					}
					if v := dist[u] + e.cost; v < nd[e.to] {
						nd[e.to] = v
						pvV[e.to] = u
						pvE[e.to] = ei
						changed = true
					}
				}
			}
			copy(dist, nd)
			if !changed {
				break
			}
		}
		if pvV[t] < 0 {
			return
		}
		if !allowAll && dist[t] >= 0 {
			return
		}
		// 前驱链必须确实从 s 可达 t；若残留陈旧前驱则放弃。
		if !pvChainValid(pvV, s, t) {
			return
		}
		bot := flowInf
		for v := t; v != s; v = pvV[v] {
			if c := fn.g[pvV[v]][pvE[v]].cap; c < bot {
				bot = c
			}
		}
		for v := t; v != s; v = pvV[v] {
			e := &fn.g[pvV[v]][pvE[v]]
			e.cap -= bot
			fn.g[v][e.rev].cap += bot
		}
	}
}

func pvChainValid(pvV []int, s, t int) bool {
	seen := map[int]bool{t: true}
	for v := t; v != s; {
		p := pvV[v]
		if p < 0 || seen[p] {
			return false
		}
		seen[p] = true
		v = p
	}
	return true
}

// lexResolve 在不改变最优基数与总权重的前提下，按边键字典序逐条尝试强制包含：
// 若包含该边后仍能达到同一最优值则永久包含，否则永久排除。
// 等价于取“按字典序列出的匹配边序列”最小的最优解，得到唯一稳定平局解。
func (sp *solveSpec) lexResolve(best map[int]struct{}) map[int]struct{} {
	bestK, bestW := sp.score(best)
	for eid := range sp.edges {
		sp.include[eid] = struct{}{}
		cand, err := sp.run()
		keepIncluded := false
		if err == nil {
			k, w := sp.score(cand)
			keepIncluded = k == bestK && w == bestW
		}
		if !keepIncluded {
			delete(sp.include, eid)
			sp.exclude[eid] = struct{}{}
		}
	}
	final, err := sp.run()
	if err != nil {
		return best
	}
	return final
}

// score 统计匹配基数与总权重。
func (sp *solveSpec) score(used map[int]struct{}) (int, int64) {
	var w int64
	for eid := range used {
		w += sp.edges[eid].Weight
	}
	return len(used), w
}

// toResult 从匹配边下标集合构造对用户可见的稳定有序结果。
func (sp *solveSpec) toResult(version int64, used map[int]struct{}) *Result {
	matchedLeft := make(map[string]struct{})
	matchedRight := make(map[string]struct{})
	matched := make([]Match, 0, len(used))
	var total int64
	for eid := range used {
		e := sp.edges[eid]
		matched = append(matched, Match{U: e.U, V: e.V, Weight: e.Weight})
		matchedLeft[e.U] = struct{}{}
		matchedRight[e.V] = struct{}{}
		total += e.Weight
	}
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].U != matched[j].U {
			return matched[i].U < matched[j].U
		}
		return matched[i].V < matched[j].V
	})
	unL := make([]string, 0)
	unR := make([]string, 0)
	for _, v := range sp.lefts {
		if _, ok := matchedLeft[v]; !ok {
			unL = append(unL, v)
		}
	}
	for _, v := range sp.rights {
		if _, ok := matchedRight[v]; !ok {
			unR = append(unR, v)
		}
	}
	return &Result{
		Version:        version,
		Mode:           sp.mode,
		Matched:        matched,
		UnmatchedLeft:  unL,
		UnmatchedRight: unR,
		Cardinality:    len(matched),
		TotalWeight:    total,
	}
}
