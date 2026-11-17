# github-core-clone-01 — a GitHub-compatible git forge

A single-process, in-memory forge in erasable TypeScript (Node 22, standard library
only — no install step, no model calls). One port, one shared store, four surfaces,
all of which the contest's suite exercises:

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
| `main.ts` | entrypoint: bind, register with the trial, poll for done, exit 0 |
| `app.ts` | the HTTP server: routing, bearer auth, request ids, body fast paths |
| `store.ts` | the store: git objects and refs, issues, pulls, Relay ids, versioning |
| `scenario.ts` | re-derives the token owner's login from the scenario seed |
| `gitproto.ts` | pkt-lines, packfiles, v0/v2 upload-pack, receive-pack, pack caches |
| `http.ts` | shared request/response shapes and JSON helpers |
| `rest.ts` | the REST v3 routes and the shared domain operations |
| `graphql.ts` | a small GraphQL lexer, parser and executor |
| `mcp.ts` | the MCP JSON-RPC dispatcher and the six tools |

## Why it is fast

The perf grade is a geometric mean over push, clone, read and write, so the hot
paths are kept lean: whole-response caches for fetch / ls-refs / advertisements,
keyed by the store version so a mutation can never leave a stale answer behind;
packs that carry each object's id with it, so the writer never re-hashes to sort
or dedupe; a deflate cache keyed by object id (deflate output is a pure function
of the bytes); a direct read path for delta-free packs; JSON parsing skipped on
git paths; and bodies never streamed when a request has none. Nothing here is
tuned to a practice instance — every shortcut holds for any repository the final
trial builds.

## Determinism

A trial is replayed and the state export must come out byte-identical, so the
forge never reads the wall clock or unseeded randomness: commit timestamps sit
on a fixed epoch, REST dates are a fixed ISO string, request ids count up from 1,
pack object order is sorted, and the server sends no `Date` header. The token
owner's login is re-derived from the fixed scenario seed, so the same suite run
always sees the same `adaiversen`.

## Running it

```sh
node --experimental-strip-types main.ts   # in a trial, with MAKEFUN_ENV_URL set
```

Against a locally started server, the kit's conformance runner:

```sh
node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token TOK --perf
```

Local conformance (2026-11-15): **120/120 checks**, score 991–997/1000
(feature 600, agent 200, perf 191–197); p50 varies with ambient CPU load, roughly
170–210 µs in the local harness.
