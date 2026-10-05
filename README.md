# Bipartite Matching Service

A local, dependency-free bipartite matching service. Inputs are two ordered
vertex groups (`left`, `right`) with weighted edges. Edges can be added,
deleted and reweighted through private transactions; every committed state is
solved incrementally, versioned, and persisted to local disk.

No external services are required — storage is an append-only write-ahead log
plus fsync/rename based snapshots in a local directory.

## Matching semantics

Three configurable solve scopes are supported:

- `max_weight` — maximize the total weight of selected edges. Vertices may
  stay unmatched. A non-positive-contribution edge is never selected (negative
  edges are dropped; a zero-weight edge is only selected if tie preference
  includes it without changing the objective).
- `max_cardinality` — first maximize the number of matched edges, then the
  total weight among maximum-cardinality solutions.
- `perfect` — every vertex on both sides must be matched (generalized
  b-matching under per-vertex caps). If the sides have different sizes or
  caps, vertices with a cap greater than one may cover several counterparts.

Every result separates matched edges (`matched`, each carrying its weight)
from unmatched vertices (`unmatched_left`, `unmatched_right`).

### Determinism and tie breaking

The same input always produces the same result. When several edge sets share
the optimal objective value, ties are resolved by a fixed lexicographic rule
over pairs sorted by `(left, right)`:

- for cardinality-fixed scopes (`perfect`, `max_cardinality`) the solution
  prefers including the earliest pair in canonical order;
- for `max_weight` the rule prefers shedding an objective-neutral edge, and
  otherwise includes the earliest canonical pair.

This is implemented as a two-stage integer min-cost flow: the primary
objective is optimized first with successive shortest paths / negative-cycle
cancellation, then an infinitesimal integer tie code is layered on pair-arc
costs and improved with residual cycles. Because the tie magnitude is strictly
smaller than one unit of the primary objective, a tie move can never change
the objective value. `math/big` integers are used so no floating-point or
overflow ambiguity is possible.

## Constraints

- **Forbidden pairs** — a forbidden edge is removed from the flow network and
  can never be matched.
- **Vertex caps** — each vertex has a default match-count cap of 1. A cap may
  be raised (b-matching: one vertex covers several counterparts) or set to 0.
  Negative caps are rejected as invalid input.

When a constraint makes a requested scope infeasible, the service returns a
typed error carrying a stable code:

- `perfect_unreachable` — no perfect matching satisfies the edges/forbidden
  pairs/caps;
- `cap_infeasible` — a vertex that must be matched (perfect scope) has cap 0.

Constraints are never silently relaxed and no partial result is returned for
an infeasible scope. Other scopes of the same version still solve normally
(e.g. forbidding edges may make `perfect` infeasible while `max_weight` still
has a valid answer).

## Updates, isolation and consistency

- Edges/constraints are changed inside a transaction (`BeginUpdate` …
  `Commit`/`Rollback`). A transaction works on a private deep copy, so
  uncommitted changes are invisible to every query.
- Commits are serialized. Each commit solves all three scopes incrementally
  from the previous committed flow, persists the new version, and only then
  publishes an immutable snapshot to readers.
- Overlapping transactions never lose committed changes. Every mutation is
  journaled; if other transactions committed after a transaction began, its
  journal is replayed onto the latest committed state at commit time. A
  successful commit is therefore always equivalent to applying the
  transaction's operations in commit (serial) order — when two transactions
  touch the same edge or constraint, the later committer's value wins,
  bit-for-bit identical to serial execution.
- When a stale transaction cannot be replayed safely (e.g. it re-adds an
  edge another commit already added, or reweights an edge another commit
  deleted), the commit is rejected with `ErrTxnConflict` — a distinguishable
  (`errors.Is`), retryable error. Nothing is published and no version number
  is consumed. The rejected transaction can never commit afterwards; the
  caller opens a fresh transaction on the latest version and re-applies the
  intended changes (staging-time validation then reflects the new state).
- Incremental results are bit-for-bit identical to a from-scratch
  recomputation (verified exhaustively in the tests by comparing the warm
  solver against a cold solve after thousands of random changes).
- A single edge change only reroutes the affected flow (local negative-cycle
  cancellation plus a bounded number of shortest paths); it does not rebuild
  the solution from scratch. The benchmark in
  `matching/perf_test.go` asserts the incremental re-solve is faster than the
  full solve on a large graph.

## Versioned snapshot reads

Commits are assigned monotonically increasing versions starting at 1. Every
query targets either the latest version (`version=0`) or an explicit
historical version. A response always reflects one complete version — graph
edges and matching result can never be mixed across versions. Reads are
lock-free against updates (atomic pointer to an immutable snapshot).

### History retention

The service retains a bounded window of committed versions for historical
reads: between `N` and `2N` recent versions, with `N` defaulting to 128 and
configurable via `OpenWithHistoryLimit` (or `-history` on the server). The
window is a deterministic function of the commit sequence and the limit, so
the readable range is identical before and after a restart — any version
readable before shutdown is readable after reopening, and pruned versions
always report `ErrUnknownVersion` (HTTP `404`). Use the same limit across
restarts to keep the window stable.

## Persistence and crash recovery

Each committed version is appended as a length-framed JSON record to
`<data-dir>/wal.log` and `fsync`ed before the version becomes visible.

- Recovery replays the WAL on open. A trailing record left torn by a crash is
  detected by its frame length/terminator, ignored, and truncated, so only
  fully committed versions survive.
- An uncommitted transaction holds no on-disk state, so a crash cannot leave a
  partially applied update.
- All retained versions are restored for historical reads, and the
  warm-solver state is rebuilt through them, so post-restart updates remain
  incremental and bit-for-bit equal to cold recomputation.
- The WAL is compacted periodically by writing a temp file, fsyncing it, and
  atomically renaming it over the live log, so compaction cannot lose commits.
- Data directories written by older versions open directly; no manual
  migration is needed.

## Concurrency

- Multiple writers are serialized through a single commit mutex. Overlapping
  transactions are merged by journal replay (see above) or rejected with
  `ErrTxnConflict`; a commit that returns success never loses its changes
  and the final state equals the serial application of all successful
  commits in commit order.
- Readers never hold the writer lock; they observe immutable snapshots and are
  not blocked for long by an in-progress update.

## HTTP usage

Run the local server (`-history` sets the retained-version window):

```sh
go run ./cmd/bimatch -addr 127.0.0.1:8080 -data ./.bimatch-data -history 128
```

Endpoints (all mutations are `POST` JSON and commit immediately):

- `GET /health` — service status and current version.
- `GET /graph?version=0` — the committed graph (latest or historical).
- `POST /edges/add` — `{ "left","right","weight" }`.
- `POST /edges/delete` — `{ "left","right" }`.
- `POST /edges/weight` — `{ "left","right","weight" }`.
- `POST /constraints/forbid` / `constraints/allow` — `{ "left","right" }`.
- `POST /constraints/cap` — `{ "vertex","side":"left|right","cap" }`.
- `GET /match?scope=max_weight|max_cardinality|perfect&version=0`.

An infeasible scope returns HTTP `409` with `error_code` set. A commit that
conflicts with concurrently committed state also returns HTTP `409` (retry
by re-applying the change on the latest state). A pruned or unknown version
returns HTTP `404`.

Example:

```sh
curl -XPOST localhost:8080/edges/add -d '{"left":"a","right":"x","weight":10}'
curl 'localhost:8080/match?scope=perfect'
```

## Package layout

- `matching/` — graph/constraint types, deterministic min-cost-flow solver,
  and the incremental warm solver.
- `store/` — framed WAL, torn-tail recovery, compaction, versioned reads.
- root package (`service.go`) — transactions, concurrent commits, immutable
  snapshots, recovery.
- `cmd/bimatch/` — the stdlib-only HTTP server.

## Tests

- `matching/solver_test.go`, `matching/solver_extra_test.go` — all scopes,
  deterministic ties, forbidden pairs, caps, infeasibility codes.
- `matching/incremental_test.go`, `matching/stress_test.go` — warm results
  equal cold recomputation after thousands of random edge/cap/forbidden
  changes for every scope.
- `matching/perf_test.go` — incremental single-change cost beats full solve.
- `store/store_test.go` — persistence across reopen, torn-tail recovery.
  `store/prune_test.go` — retention window, configurable history limit.
- `service_test.go` — query semantics, transaction isolation, versioned
  reads, constraint failure codes, crash recovery, concurrent readers/writers.
- `service_conflict_test.go` — overlapping transactions on different/same
  data, conflict rejection and retry, rejected-transaction reuse, rollback
  interleaving, concurrent commit pressure (run with `-race`).
- `service_history_test.go` — bit-for-bit history comparison across
  restarts, retention-window consistency, post-restart incremental
  correctness, legacy data-directory compatibility.

Run them with:

```sh
go test -race ./...
```
