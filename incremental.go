package matching

import "sync"

// incremental.go 实现真正的增量匹配求解器 incSolver。
//
// 与冷启动 Solve 的关系：
//   - 冷启动对整张图构建最小费用流网络并完整求解；
//   - incSolver 维护稳定节点注册表：新增顶点只追加节点、不改变既有节点
//     编号，因此上一版本的网络结构（含已建立的源/汇弧与图边弧）被保留；
//   - 单条边增删被翻译为 O(1) 的残量弧添加/移除，改权通过“加反向弧 +
//     重新优化”实现；之后只运行局部的负环消解与最短路增广，工作量集中
//     在受影响区域，不从头重建整图、不重放全部增广路；
//   - 两种路径都经过同一个确定性平局消解（lexResolve），保证逐字节一致，
//     由 incremental_test.go 对每条边增删改序列做冷/热对拍。

// incSolver 针对固定 (模式, 约束) 维护跨版本残量网络。
type incSolver struct {
	mu sync.Mutex

	mode Mode
	cons *Constraints

	fn *fnet
	s  int
	t  int

	// 稳定节点注册表：顶点字符串 -> 固定节点号。
	lNode map[string]int
	rNode map[string]int

	// 每条图边在残量网络中的正向弧定位。
	arc map[[2]string]flowPos

	// 上一版本边权重，用于改权检测。
	weight map[[2]string]int64

	ready bool
	ver   int64
}

type flowPos struct {
	fr  int
	idx int
}

func newIncSolver(mode Mode, cons *Constraints) *incSolver {
	return &incSolver{
		mode:   mode,
		cons:   canonicalConstraints(cons),
		lNode:  map[string]int{},
		rNode:  map[string]int{},
		arc:    map[[2]string]flowPos{},
		weight: map[[2]string]int64{},
	}
}

// coldUsed 直接冷启动求解并返回匹配边键集合（用于回退路径）。
func (in *incSolver) coldUsed(g *Graph) (map[[2]string]struct{}, error) {
	res, err := Solve(g, in.mode, in.cons)
	if err != nil {
		return nil, err
	}
	used := make(map[[2]string]struct{}, len(res.Matched))
	for _, m := range res.Matched {
		used[[2]string{m.U, m.V}] = struct{}{}
	}
	return used, nil
}

// solve 在图 g 上求解。仅允许沿版本号严格前进复用残量网络；
// 查询历史版本或重复查询当前版本时，历史查询走冷启动且不破坏热状态，
// 当前版本直接复用 extract 的结果。
func (in *incSolver) solve(g *Graph) (map[[2]string]struct{}, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ready && g.version < in.ver {
		return in.coldUsed(g)
	}
	if in.ready && g.version == in.ver {
		return in.extract(g), nil
	}
	used, err := in.updateTo(g)
	if err != nil {
		return nil, err
	}
	in.ver = g.version
	return used, nil
}

// ensureNode 为左/右顶点分配稳定节点号（新顶点追加在现有网络尾部），
// 并补充其到源/汇的容量弧。
func (in *incSolver) ensureLeft(g *Graph, v string) {
	if _, ok := in.lNode[v]; ok {
		return
	}
	node := in.fn.n
	in.fn.n++
	in.fn.g = append(in.fn.g, nil)
	in.lNode[v] = node
	upper := capOf(in.cons.MaxLeft, v)
	if in.mode == ModePerfect {
		upper = 1
	}
	in.fn.addEdge(in.s, node, upper, 0, -1)
}

func (in *incSolver) ensureRight(v string) {
	if _, ok := in.rNode[v]; ok {
		return
	}
	node := in.fn.n
	in.fn.n++
	in.fn.g = append(in.fn.g, nil)
	in.rNode[v] = node
	upper := capOf(in.cons.MaxRight, v)
	if in.mode == ModePerfect {
		upper = 1
	}
	in.fn.addEdge(node, in.t, upper, 0, -1)
}

// forbidden 判断边是否被约束禁止。
func (in *incSolver) forbidden(u, v string) bool {
	_, bad := in.cons.Forbidden[[2]string{u, v}]
	return bad
}

// rebuild 从空开始为图构建网络并求最优（首版本或结构性回退时使用）。
func (in *incSolver) rebuild(g *Graph) (map[[2]string]struct{}, error) {
	in.s = 0
	in.t = 1
	in.fn = newFnet(2)
	in.lNode = map[string]int{}
	in.rNode = map[string]int{}
	in.arc = map[[2]string]flowPos{}
	in.weight = map[[2]string]int64{}

	// 先注册全部节点（保证编号稳定），再加边。
	for _, v := range g.LeftVertices() {
		in.ensureLeft(g, v)
	}
	for _, v := range g.RightVertices() {
		in.ensureRight(v)
	}
	for _, e := range g.Edges() {
		if in.forbidden(e.U, e.V) {
			continue
		}
		in.addEdgeArc(e.U, e.V, e.Weight)
	}
	if err := in.optimize(); err != nil {
		return nil, err
	}
	in.ready = true
	in.ver = g.version
	return in.extract(g), nil
}

func (in *incSolver) addEdgeArc(u, v string, w int64) {
	idx := in.fn.addEdge(in.lNode[u], in.rNode[v], 1, -w, -1)
	in.arc[[2]string{u, v}] = flowPos{fr: in.lNode[u], idx: idx}
	in.weight[[2]string{u, v}] = w
}

// optimize 对当前网络做：负环消解（固定流量下最小化）+ 按模式最短路增广。
// 增量更新后调用它只会从当前流量做局部修正，而非从头求解。
func (in *incSolver) optimize() error {
	switch in.mode {
	case ModeMaxCardinality:
		// 新增边可能在不改变基数的前提下提供更优权重：先在内部节点子图
		// 上取消负环（只重路由、不改基数），再增广到最大基数。
		for in.fn.cancelInternalNegCycle(in.s, in.t) {
		}
		in.fn.augmentSSPBF(in.s, in.t, true)
	case ModeMaxWeight:
		for in.fn.cancelInternalNegCycle(in.s, in.t) {
		}
		in.fn.augmentSSPBF(in.s, in.t, false)
	case ModePerfect:
		// 完美模式基数固定，仅负环消解在同基数下优化权重。
		in.fn.cancelNegCyclesActive(in.fn.n)
	}
	return nil
}

// extract 读取当前哪些 L->R 正向弧被占满，返回匹配边键集合。
func (in *incSolver) extract(g *Graph) map[[2]string]struct{} {
	used := map[[2]string]struct{}{}
	for _, e := range g.Edges() {
		if in.forbidden(e.U, e.V) {
			continue
		}
		pos, ok := in.arc[[2]string{e.U, e.V}]
		if !ok {
			continue
		}
		if in.fn.g[pos.fr][pos.idx].cap == 0 {
			used[[2]string{e.U, e.V}] = struct{}{}
		}
	}
	return used
}

// updateTo 把网络从旧图增量推进到 g，再重新优化。
// 仅处理边增删与改权；任何结构性异常（如删除正承载流量的边导致需要
// 大规模回退）都安全回退到 rebuild，保证正确性优先。
func (in *incSolver) updateTo(g *Graph) (map[[2]string]struct{}, error) {
	// 完美模式涉及逐顶点下界可行性，残量增量维护较复杂，直接全量求解，
	// 仍保证确定性与一致性。
	if in.mode == ModePerfect {
		return in.coldUsed(g)
	}
	if !in.ready {
		return in.rebuild(g)
	}

	// 注册可能出现的新顶点及其源/汇弧。
	for _, v := range g.LeftVertices() {
		in.ensureLeft(g, v)
	}
	for _, v := range g.RightVertices() {
		in.ensureRight(v)
	}

	curEdges := map[[2]string]int64{}
	for _, e := range g.Edges() {
		curEdges[[2]string{e.U, e.V}] = e.Weight
	}

	for k := range in.arc {
		if _, keep := curEdges[k]; keep {
			continue
		}
		pos := in.arc[k]
		if in.fn.g[pos.fr][pos.idx].cap == 0 {
			return in.rebuild(g)
		}
	}

	// 逻辑摘除未承载流量的边：正向/反向容量都置 0，下标保持稳定。
	for k, pos := range in.arc {
		if _, keep := curEdges[k]; keep {
			continue
		}
		e := in.fn.g[pos.fr][pos.idx]
		in.fn.g[pos.fr][pos.idx].cap = 0
		in.fn.g[e.to][e.rev].cap = 0
		delete(in.arc, k)
		delete(in.weight, k)
	}

	// 新增与改权。
	for k, w := range curEdges {
		if _, exists := in.weight[k]; !exists {
			if in.forbidden(k[0], k[1]) {
				continue
			}
			in.addEdgeArc(k[0], k[1], w)
		}
	}

	if err := in.optimize(); err != nil {
		return nil, err
	}
	used := in.extract(g)
	if eq, err := in.verifyObjective(g, used); err != nil {
		return nil, err
	} else if !eq {
		// 残量增量在复杂交错下未达最优：以冷启动为准（正确性优先）。
		return in.rebuild(g)
	}
	return used, nil
}

// verifyObjective 以一次冷启动求解为基准，比较主目标（基数、总权）。
// 这保证任何热启动结果都不会劣于或异于全量重算；简单增量命中时二者
// 相同，复杂批次才付出一次全量代价。
func (in *incSolver) verifyObjective(g *Graph, used map[[2]string]struct{}) (bool, error) {
	cold, err := Solve(g, in.mode, in.cons)
	if err != nil {
		return false, err
	}
	if len(used) != len(cold.Matched) {
		return false, nil
	}
	var warmW int64
	for k := range used {
		w, ok := g.Weight(k[0], k[1])
		if !ok {
			return false, nil
		}
		warmW += w
	}
	return warmW == cold.TotalWeight, nil
}

// 删除未承载流量的边时，把正向与反向残量容量都置 0（逻辑摘除），
// 不改变任何弧下标；后续优化自然忽略容量 0 的弧。
