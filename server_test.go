package matching

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(t *testing.T) (*Server, *Service) {
	t.Helper()
	svc, err := NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Close() })
	return NewServer(svc), svc
}

func postJSON(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestHTTPEndToEnd(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	code, out := postJSON(t, h, "/update", map[string]any{
		"upsert": []map[string]any{
			{"u": "a", "v": "x", "weight": 5},
			{"u": "a", "v": "y", "weight": 9},
			{"u": "b", "v": "x", "weight": 8},
			{"u": "b", "v": "y", "weight": 1},
		},
	})
	if code != 200 || out["version"].(float64) != 1 {
		t.Fatalf("update code=%d out=%v", code, out)
	}

	code, out = postJSON(t, h, "/query", map[string]any{"mode": "max_weight"})
	if code != 200 {
		t.Fatalf("query code=%d out=%v", code, out)
	}
	if out["total_weight"].(float64) != 17 || out["cardinality"].(float64) != 2 {
		t.Fatalf("query out=%v", out)
	}
	matched := out["matched"].([]any)
	if len(matched) != 2 {
		t.Fatalf("matched=%v", matched)
	}

	// 约束使结果受限但可行。
	code, out = postJSON(t, h, "/query", map[string]any{
		"mode":      "max_weight",
		"forbidden": []map[string]string{{"u": "a", "v": "y"}},
	})
	if code != 200 {
		t.Fatalf("forbidden query code=%d out=%v", code, out)
	}
	for _, m := range out["matched"].([]any) {
		mm := m.(map[string]any)
		if mm["u"] == "a" && mm["v"] == "y" {
			t.Fatal("forbidden pair returned")
		}
	}
}

func TestHTTPInfeasibleReason(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	postJSON(t, h, "/update", map[string]any{
		"upsert": []map[string]any{
			{"u": "a", "v": "x", "weight": 1},
			{"u": "b", "v": "x", "weight": 2},
		},
	})
	code, out := postJSON(t, h, "/query", map[string]any{"mode": "perfect"})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["reason"] != string(FailurePerfectInfeasible) {
		t.Fatalf("reason=%v", out["reason"])
	}
}

func TestHTTPInvalidConstraint(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	postJSON(t, h, "/update", map[string]any{
		"upsert": []map[string]any{{"u": "a", "v": "x", "weight": 1}},
	})
	code, out := postJSON(t, h, "/query", map[string]any{
		"mode":     "max_weight",
		"max_left": map[string]int{"a": 0},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["reason"] != string(FailureInvalidConstraint) {
		t.Fatalf("reason=%v", out["reason"])
	}
}

func TestHTTPVersionedRead(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	postJSON(t, h, "/update", map[string]any{
		"upsert": []map[string]any{{"u": "a", "v": "x", "weight": 5}},
	})
	postJSON(t, h, "/update", map[string]any{
		"upsert": []map[string]any{{"u": "a", "v": "x", "weight": 50}},
	})
	code, out := postJSON(t, h, "/query", map[string]any{"version": 1})
	if code != 200 || out["total_weight"].(float64) != 5 {
		t.Fatalf("v1 read out=%v code=%d", out, code)
	}
}
