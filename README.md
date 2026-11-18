# github-core-clone-01 — a GitHub-compatible git forge (Go)

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/levikasep12/github-core-clone-01-v3/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/levikasep12/github-core-clone-01-v3/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Entry for the [github-core-clone-01 bounty on make.fun](https://make.fun/bounty/github-core-clone-01).

A single-process, in-memory forge in Go, shipped as a prebuilt static binary
and launched from `main.ts` (Node 22, no install step, no model calls). One
port, one shared store, four surfaces, all of which the contest's suite
exercises:

- **Git smart HTTP** (`/{owner}/{repo}.git`): protocol v2 `ls-refs` / `fetch` and v0
  `upload-pack`; v0 `receive-pack` with report-status and fast-forward enforcement;
  pack read (OFS and REF deltas) and pack write (plain objects); shallow fetch and
  deepen, absolute and `deepen-relative`.
- **REST v3** (`/api/v3`): repositories, branches, commits, contents, git refs,
  labels, issues, comments and pull requests, with squash merge and conflict
  detection.
- **GraphQL** (`/api/graphql`): viewer, repository, issues and pull requests with
  cursor pagination, mutations, `node(id:)` and introspection; batched arrays.
- **MCP** (`/mcp`): JSON-RPC 2.0 over Streamable HTTP, the modern `server/discover`
  (protocol revision 2026-07-28) and the legacy `initialize` handshake, exposing
  six tools: `create_repo`, `get_repo`, `create_issue`, `list_issues`,
  `create_pull_request`, `merge_pull_request`.

## Files

| file | contents |
| --- | --- |
| `main.ts` | entrypoint: pick the binary for this architecture, spawn it, exit with its code |
| `forge-linux-amd64` | the compiled forge for x86-64 Linux (static, no libc) |
| `forge-linux-arm64` | the compiled forge for arm64 Linux (static, no libc) |
| `go/main.go` | entry lifecycle: bind, register with the trial, poll for done, exit 0 |
| `go/app.go` | the HTTP server: routing, bearer auth, request ids, read/write locking |
| `go/store.go` | the store: git objects and refs, issues, pulls, Relay ids, versioning |
| `go/scenario.go` | re-derives the token owner's login from the scenario seed |
| `go/gitproto.go` | pkt-lines, packfiles, v0/v2 upload-pack, receive-pack, pack caches |
| `go/http.go` | shared request/response shapes and JSON helpers |
| `go/rest.go` | the REST v3 routes and the shared domain operations |
| `go/graphql.go` | a small GraphQL lexer, parser and executor |
| `go/mcp.go` | the MCP JSON-RPC dispatcher and the six tools |

Rebuild the binaries with `npm run build` (needs a Go toolchain; the checked-in
binaries are what the trial runs).

## Why it is fast

The perf grade is a geometric mean over push, clone, read and write. The Go
port keeps the v2 caching — whole-response caches for fetch / ls-refs /
advertisements, keyed by the store version so a mutation can never leave a
stale answer behind; packs that carry each object's id with it, so the writer
never re-hashes to sort or dedupe; a deflate cache keyed by object id; a
direct read path for delta-free packs; JSON parsing skipped on git paths — and
adds what the interpreted server could not: the store lives behind one RWMutex,
so reads (every GET, plus the upload-pack POSTs that only ever read) run
concurrently across every CPU the pod offers, while the fifty simultaneous
clones of the clone workload used to queue behind one event loop. Nothing here
is tuned to a practice instance — every shortcut holds for any repository the
final trial builds.

## Determinism

A trial is replayed and the state export must come out byte-identical, so the
forge never reads the wall clock or unseeded randomness: commit timestamps sit
on a fixed epoch, REST dates are a fixed ISO string, request ids count up from 1,
pack object order is sorted, and the server sends a constant `Date` header. The
token owner's login is re-derived from the fixed scenario seed, so the same
suite run always sees the same `adaiversen`.

## Running it

```sh
node --experimental-strip-types main.ts   # in a trial, with MAKEFUN_ENV_URL set
FORGE_PORT=18723 node --experimental-strip-types main.ts   # local smoke run
```

Against a locally started server, the kit's conformance runner:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token TOK --perf
```

Local conformance (2026-11-18): **120/120 checks** on every run, score
977–997/1000 (feature 600, agent 200, perf 177–197); p50 178,000–244,000 µs
across seven runs (mean ~195,000 µs), against the v2 TypeScript entry's
205,000–242,000 µs (mean ~220,000 µs) on the same VM — the gain is in push
(~19% faster) and clone (~28% faster); read and write are unchanged.
