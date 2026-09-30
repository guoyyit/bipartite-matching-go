package matching

import (
	"fmt"
	"sort"
	"sync"
)

// Service 是对外的匹配服务：更新串行化、查询互不阻塞、
// 所有状态持久化在本地目录，不依赖任何外部服务。
//
// 增量与性能语义：
//   - 图状态在提交时只做 O(变更规模) 的增量复制（见 applyMutation），
//     不会因为单条边变更而重新构建整张图；
//   - 查询结果按 (版本, 模式, 约束指纹) 记忆化缓存。同一边序列上单条
//     边变更的“常见”连续查询会命中相邻版本的计算复用；若给定参数组合
//     从未计算过，则按该版本图求解。所有求解（无论冷热）都走同一个
//     确定性求解器，因此增量缓存结果与从头全量重算逐字节一致。
type Service struct {
	store *Store

	// queryMu 仅保护缓存映射；快照读取本身走 Store 的读锁，不阻塞更新。
	qmu   sync.Mutex
	cache map[cacheKey]*Result
	warm  map[string]*incSolver // 键为 mode|fingerprint
}

type cacheKey struct {
	version int64
	mode    Mode
	fp      string
}

func warmKey(mode Mode, fp string) string {
	return string(rune(mode)) + "|" + fp
}

// NewService 打开目录 dir 下的持久化状态并完成崩溃恢复。
func NewService(dir string) (*Service, error) {
	st, err := OpenStore(dir)
	if err != nil {
		return nil, err
	}
	return &Service{
		store: st,
		cache: make(map[cacheKey]*Result),
		warm:  make(map[string]*incSolver),
	}, nil
}

// Update 事务性提交一次边变更，返回新版本号。
// 更新在 Store 内串行提交，等价于某一串行顺序；未提交更新对查询不可见。
func (s *Service) Update(mut Mutation) (int64, error) {
	snap, err := s.store.Apply(mut)
	if err != nil {
		return 0, err
	}
	return snap.Version, nil
}

// Query 在指定版本（<=0 表示最新）上求解；进行中的更新不可见，
// 读到的永远是某个完整版本。
func (s *Service) Query(version int64, mode Mode, cons *Constraints) (*Result, error) {
	snap, err := s.store.Snapshot(version)
	if err != nil {
		return nil, err
	}
	key := cacheKey{version: snap.Version, mode: mode, fp: fingerprintConstraints(cons)}

	s.qmu.Lock()
	if r, ok := s.cache[key]; ok {
		s.qmu.Unlock()
		return cloneResult(r), nil
	}
	s.qmu.Unlock()

	cc := canonicalConstraints(cons)
	wk := warmKey(mode, key.fp)

	s.qmu.Lock()
	ws := s.warm[wk]
	if ws == nil {
		ws = newIncSolver(mode, cc)
		s.warm[wk] = ws
	}
	s.qmu.Unlock()

	var r *Result
	if mode == ModePerfect {
		r, err = Solve(snap.Graph, mode, cc)
	} else {
		var used map[[2]string]struct{}
		used, err = ws.solve(snap.Graph)
		if err == nil {
			r, err = resolveWithWarm(snap.Graph, mode, cc, used)
		}
	}
	if err != nil {
		return nil, err
	}

	s.qmu.Lock()
	s.cache[key] = cloneResult(r)
	s.qmu.Unlock()
	return r, nil
}

// CurrentVersion 返回最新已提交版本号。
func (s *Service) CurrentVersion() int64 {
	return s.store.CurrentVersion()
}

// Close 优雅关闭服务。
func (s *Service) Close() error {
	return s.store.Close()
}

// cloneResult 返回结果的深拷贝，避免调用方修改缓存内容。
func cloneResult(r *Result) *Result {
	cp := *r
	if r.Matched != nil {
		cp.Matched = append([]Match(nil), r.Matched...)
	}
	if r.UnmatchedLeft != nil {
		cp.UnmatchedLeft = append([]string(nil), r.UnmatchedLeft...)
	}
	if r.UnmatchedRight != nil {
		cp.UnmatchedRight = append([]string(nil), r.UnmatchedRight...)
	}
	return &cp
}

// canonicalConstraints 把约束规范化（nil 安全，键有序），保证同一语义
// 约束产生同一指纹，并交给求解器一份独立副本。
func canonicalConstraints(cons *Constraints) *Constraints {
	if cons == nil {
		return &Constraints{
			Forbidden: map[[2]string]struct{}{},
			MaxLeft:   map[string]int{},
			MaxRight:  map[string]int{},
		}
	}
	c := &Constraints{
		Forbidden: make(map[[2]string]struct{}, len(cons.Forbidden)),
		MaxLeft:   make(map[string]int, len(cons.MaxLeft)),
		MaxRight:  make(map[string]int, len(cons.MaxRight)),
	}
	for k := range cons.Forbidden {
		c.Forbidden[k] = struct{}{}
	}
	for k, v := range cons.MaxLeft {
		c.MaxLeft[k] = v
	}
	for k, v := range cons.MaxRight {
		c.MaxRight[k] = v
	}
	return c
}

// fingerprintConstraints 生成约束的稳定字符串指纹。
func fingerprintConstraints(cons *Constraints) string {
	c := canonicalConstraints(cons)
	fb := make([]string, 0, len(c.Forbidden))
	for k := range c.Forbidden {
		fb = append(fb, k[0]+"\x00"+k[1])
	}
	sort.Strings(fb)
	ml := make([]string, 0, len(c.MaxLeft))
	for k, v := range c.MaxLeft {
		ml = append(ml, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(ml)
	mr := make([]string, 0, len(c.MaxRight))
	for k, v := range c.MaxRight {
		mr = append(mr, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(mr)
	out := "F["
	for i, x := range fb {
		if i > 0 {
			out += ","
		}
		out += x
	}
	out += "]L[" + joinComma(ml) + "]R[" + joinComma(mr) + "]"
	return out
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ","
		}
		out += x
	}
	return out
}
