package matching

import (
	"encoding/json"
	"net/http"
)

// Server 把 Service 暴露为本地 HTTP 服务，仅监听本机回环，
// 不依赖任何外部服务。
type Server struct {
	svc *Service
	mux *http.ServeMux
}

// NewServer 创建包装 Service 的本地 HTTP 服务。
func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回 HTTP 路由处理器。
func (s *Server) Handler() http.Handler {
	return s.mux
}

type edgeDTO struct {
	U      string `json:"u"`
	V      string `json:"v"`
	Weight int64  `json:"weight"`
}

type keyDTO struct {
	U string `json:"u"`
	V string `json:"v"`
}

type updateRequest struct {
	Upsert []edgeDTO `json:"upsert"`
	Delete []keyDTO  `json:"delete"`
}

type queryRequest struct {
	Version   int64          `json:"version"`
	Mode      string         `json:"mode"`
	Forbidden []keyDTO       `json:"forbidden"`
	MaxLeft   map[string]int `json:"max_left"`
	MaxRight  map[string]int `json:"max_right"`
}

type matchDTO struct {
	U      string `json:"u"`
	V      string `json:"v"`
	Weight int64  `json:"weight"`
}

type resultDTO struct {
	Version        int64      `json:"version"`
	Mode           string     `json:"mode"`
	Matched        []matchDTO `json:"matched"`
	UnmatchedLeft  []string   `json:"unmatched_left"`
	UnmatchedRight []string   `json:"unmatched_right"`
	Cardinality    int        `json:"cardinality"`
	TotalWeight    int64      `json:"total_weight"`
}

type errorDTO struct {
	Error  string        `json:"error"`
	Reason FailureReason `json:"reason,omitempty"`
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /update", s.handleUpdate)
	s.mux.HandleFunc("POST /query", s.handleQuery)
	s.mux.HandleFunc("GET /version", s.handleVersion)
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int64{"version": s.svc.CurrentVersion()})
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSolveError(w, http.StatusBadRequest, err)
		return
	}
	mut := Mutation{}
	for _, e := range req.Upsert {
		mut.Upsert = append(mut.Upsert, Edge{U: e.U, V: e.V, Weight: e.Weight})
	}
	for _, k := range req.Delete {
		mut.Delete = append(mut.Delete, [2]string{k.U, k.V})
	}
	v, err := s.svc.Update(mut)
	if err != nil {
		writeSolveError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"version": v})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSolveError(w, http.StatusBadRequest, err)
		return
	}
	mode, err := parseMode(req.Mode)
	if err != nil {
		writeSolveError(w, http.StatusBadRequest, err)
		return
	}
	cons := &Constraints{
		Forbidden: map[[2]string]struct{}{},
		MaxLeft:   req.MaxLeft,
		MaxRight:  req.MaxRight,
	}
	for _, k := range req.Forbidden {
		cons.Forbidden[[2]string{k.U, k.V}] = struct{}{}
	}
	res, err := s.svc.Query(req.Version, mode, cons)
	if err != nil {
		// 求解不可行返回可区分原因与 422，不返回部分结果。
		if se, ok := err.(*SolveError); ok {
			writeJSON(w, http.StatusUnprocessableEntity, errorDTO{Error: se.Message, Reason: se.Reason})
			return
		}
		writeSolveError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, toResultDTO(res))
}

func parseMode(s string) (Mode, error) {
	switch s {
	case "", "max_weight":
		return ModeMaxWeight, nil
	case "max_cardinality":
		return ModeMaxCardinality, nil
	case "perfect":
		return ModePerfect, nil
	default:
		return ModeMaxWeight, &SolveError{Reason: FailureInvalidGraph, Message: "unknown mode: " + s}
	}
}

func modeName(m Mode) string {
	switch m {
	case ModePerfect:
		return "perfect"
	case ModeMaxCardinality:
		return "max_cardinality"
	default:
		return "max_weight"
	}
}

func toResultDTO(r *Result) resultDTO {
	dto := resultDTO{
		Version:        r.Version,
		Mode:           modeName(r.Mode),
		Matched:        []matchDTO{},
		UnmatchedLeft:  orEmpty(r.UnmatchedLeft),
		UnmatchedRight: orEmpty(r.UnmatchedRight),
		Cardinality:    r.Cardinality,
		TotalWeight:    r.TotalWeight,
	}
	for _, m := range r.Matched {
		dto.Matched = append(dto.Matched, matchDTO{U: m.U, V: m.V, Weight: m.Weight})
	}
	return dto
}

func orEmpty(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSolveError(w http.ResponseWriter, status int, err error) {
	if se, ok := err.(*SolveError); ok {
		writeJSON(w, status, errorDTO{Error: se.Message, Reason: se.Reason})
		return
	}
	writeJSON(w, status, errorDTO{Error: err.Error()})
}
