package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	bipartite "github.com/guoyyit/bipartite-matching-go"
	"github.com/guoyyit/bipartite-matching-go/matching"
)

type server struct {
	svc *bipartite.Service
}

type edgeReq struct {
	Left   string `json:"left"`
	Right  string `json:"right"`
	Weight int64  `json:"weight"`
}

type capReq struct {
	Vertex string `json:"vertex"`
	Cap    int    `json:"cap"`
	Side   string `json:"side"`
}

type pairReq struct {
	Left  string `json:"left"`
	Right string `json:"right"`
}

type queryResp struct {
	Version        int64            `json:"version"`
	Scope          string           `json:"scope"`
	Weight         int64            `json:"weight"`
	Cardinality    int              `json:"cardinality"`
	Matched        []matching.Match `json:"matched"`
	UnmatchedLeft  []string         `json:"unmatched_left"`
	UnmatchedRight []string         `json:"unmatched_right"`
	Error          string           `json:"error,omitempty"`
	ErrorCode      string           `json:"error_code,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	dataDir := flag.String("data", "./.bimatch-data", "local state directory")
	history := flag.Int("history", 0, "committed versions retained for historical reads (0 = default 128)")
	flag.Parse()

	svc, err := bipartite.OpenWithHistoryLimit(*dataDir, matching.NewGraph(), *history)
	if err != nil {
		log.Fatalf("open service: %v", err)
	}
	defer svc.Close()

	srv := &server{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.health)
	mux.HandleFunc("/graph", srv.graph)
	mux.HandleFunc("/edges/", srv.edge)
	mux.HandleFunc("/constraints/forbid", srv.forbid)
	mux.HandleFunc("/constraints/allow", srv.allow)
	mux.HandleFunc("/constraints/cap", srv.cap)
	mux.HandleFunc("/match", srv.match)

	log.Printf("bipartite matching service listening on %s, state in %s", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.svc.Version()})
}

func (s *server) graph(w http.ResponseWriter, r *http.Request) {
	version := int64(0)
	if v := r.URL.Query().Get("version"); v != "" {
		if _, err := fmt.Sscan(v, &version); err != nil {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
	}
	g, err := s.svc.GraphAt(version)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// edge handles POST /edges/add, POST /edges/delete, POST /edges/weight.
func (s *server) edge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post required", http.StatusMethodNotAllowed)
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/edges/")
	var req edgeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	txn := s.svc.BeginUpdate()
	p := matching.Pair{Left: req.Left, Right: req.Right}
	var err error
	switch action {
	case "add":
		err = txn.AddEdge(p, req.Weight)
	case "delete":
		err = txn.DeleteEdge(p)
	case "weight":
		err = txn.SetWeight(p, req.Weight)
	default:
		txn.Rollback()
		http.Error(w, "unknown edge action", http.StatusNotFound)
		return
	}
	if err != nil {
		txn.Rollback()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version, err := txn.Commit()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"committed_version": version})
}

func (s *server) forbid(w http.ResponseWriter, r *http.Request) {
	s.constraintPair(w, r, true)
}

func (s *server) allow(w http.ResponseWriter, r *http.Request) {
	s.constraintPair(w, r, false)
}

func (s *server) constraintPair(w http.ResponseWriter, r *http.Request, forbid bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "post required", http.StatusMethodNotAllowed)
		return
	}
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	txn := s.svc.BeginUpdate()
	p := matching.Pair{Left: req.Left, Right: req.Right}
	var err error
	if forbid {
		err = txn.Forbid(p)
	} else {
		err = txn.Allow(p)
	}
	if err != nil {
		txn.Rollback()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version, err := txn.Commit()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"committed_version": version})
}

func (s *server) cap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "post required", http.StatusMethodNotAllowed)
		return
	}
	var req capReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	txn := s.svc.BeginUpdate()
	var err error
	switch req.Side {
	case "left":
		err = txn.SetLeftCap(req.Vertex, req.Cap)
	case "right":
		err = txn.SetRightCap(req.Vertex, req.Cap)
	default:
		txn.Rollback()
		http.Error(w, "side must be left or right", http.StatusBadRequest)
		return
	}
	if err != nil {
		txn.Rollback()
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	version, err := txn.Commit()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"committed_version": version})
}

func (s *server) match(w http.ResponseWriter, r *http.Request) {
	scope := matching.ScopeMaxWeight
	switch r.URL.Query().Get("scope") {
	case "", "max_weight":
		scope = matching.ScopeMaxWeight
	case "max_cardinality":
		scope = matching.ScopeMaxCardinality
	case "perfect":
		scope = matching.ScopePerfect
	default:
		http.Error(w, "unknown scope", http.StatusBadRequest)
		return
	}
	version := int64(0)
	if v := r.URL.Query().Get("version"); v != "" {
		if _, err := fmt.Sscan(v, &version); err != nil {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
	}
	sol, err := s.svc.Query(scope, version)
	resp := queryResp{Scope: scope.String(), Version: s.svc.Version()}
	if err != nil {
		var infeasible *matching.InfeasibleError
		if errors.As(err, &infeasible) {
			resp.Error = infeasible.Message
			resp.ErrorCode = string(infeasible.Code)
			if version != 0 {
				resp.Version = version
			}
			writeJSON(w, http.StatusConflict, resp)
			return
		}
		writeError(w, err)
		return
	}
	resp.Version = s.svc.Version()
	if version != 0 {
		resp.Version = version
	}
	resp.Weight = sol.Weight
	resp.Cardinality = sol.Cardinality
	resp.Matched = sol.Matched
	resp.UnmatchedLeft = sol.UnmatchedLeft
	resp.UnmatchedRight = sol.UnmatchedRight
	writeJSON(w, http.StatusOK, resp)
}

func writeError(w http.ResponseWriter, err error) {
	if errors.Is(err, bipartite.ErrTxnConflict) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, bipartite.ErrUnknownVersion) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
