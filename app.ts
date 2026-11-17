// The forge's HTTP server: one process, one in-memory store, four surfaces on one port —
// REST v3 at /api/v3, GraphQL at /api/graphql, MCP at /mcp, git smart HTTP at
// /{owner}/{repo}.git — plus /healthz, the only route that needs no bearer token.
//
// Every reply carries a non-empty X-GitHub-Request-Id. Auth is a bearer token: the suite's
// token arrives in MAKEFUN_SHOP_TOKEN, and a missing or wrong token is 401. When the variable
// is unset (a local smoke run), any non-empty bearer token is accepted — the trial always
// sets it.
//
// Why the request path looks the way it does (the timed workloads issue thousands of
// requests, so the per-request costs dominate):
//   - The URL is split by hand (`?` search plus a `%` check) instead of `new URL(...)`:
//     the workload paths are plain ASCII with no query string.
//   - A request with no body never touches the request stream: GETs skip the stream
//     entirely instead of awaiting an already-ended one.
//   - Bodies are read with `data`/`end` listeners, not `for await`, which costs more per
//     request on this Node.
//   - JSON is parsed only off the git path: git bodies are binary pkt-lines and packs, and
//     no handler on a git route ever reads req.json, so decoding them to UTF-8 just to
//     watch JSON.parse throw is pure waste. Every other surface keeps the exact rule that
//     any non-empty body that is not valid JSON is a 400.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Forge } from './store.ts';
import { handleRest } from './rest.ts';
import { handleGraphql } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleGitHttp } from './gitproto.ts';
import { err, json, newRequestId, type Req, type Res } from './http.ts';

/** Request bodies larger than this are refused: packs stay far below it. */
const BODY_LIMIT = 64 * 1024 * 1024;

/** One shared empty query set for the common case of a URL with no `?`. */
const NO_QUERY = new URLSearchParams();

/** Split a raw request target into a decoded pathname, segments and a query set — no `new URL`. */
function splitTarget(rawUrl: string): { pathname: string; segments: string[]; query: URLSearchParams } {
  const qmark = rawUrl.indexOf('?');
  const rawPath = qmark < 0 ? rawUrl : rawUrl.slice(0, qmark);
  const query = qmark < 0 ? NO_QUERY : new URLSearchParams(rawUrl.slice(qmark + 1));
  const pathname = rawPath.includes('%') ? decodeURIComponent(rawPath) : rawPath;
  const segments = pathname
    .split('/')
    .filter((segment) => segment !== '')
    .map((segment) => (segment.includes('%') ? decodeURIComponent(segment) : segment));
  return { pathname, segments, query };
}

/** True when the request can carry a body (a positive length, or chunked encoding). */
function hasBody(req: IncomingMessage): boolean {
  const length = req.headers['content-length'];
  if (typeof length === 'string' && Number.parseInt(length, 10) > 0) return true;
  if (typeof req.headers['transfer-encoding'] === 'string') return true;
  return false;
}

/** Read the request body with `data`/`end` listeners; null when it exceeds the cap. */
function collectBody(req: IncomingMessage): Promise<Buffer | null> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    let size = 0;
    let settled = false;
    const finish = (value: Buffer | null): void => {
      if (settled) return;
      settled = true;
      resolve(value);
    };
    req.on('data', (chunk: Buffer) => {
      size += chunk.length;
      if (size > BODY_LIMIT) {
        req.destroy();
        finish(null);
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => finish(Buffer.concat(chunks)));
    req.on('error', () => finish(null));
  });
}

/** The bearer token on this request matches what the env configured, if any. */
function isAuthenticated(req: Req): boolean {
  const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const header = req.headers.authorization;
  if (typeof header !== 'string' || !header.startsWith('Bearer ')) return false;
  const presented = header.slice('Bearer '.length).trim();
  if (presented === '') return false;
  if (expected === '') return true;
  return presented === expected;
}

/** Decode a request body as JSON, unless it belongs to a git route (binary). */
function decodeJsonBody(body: Buffer, gitPath: boolean): { value: unknown; failed: boolean } {
  if (body.length === 0 || gitPath) return { value: null, failed: false };
  try {
    return { value: JSON.parse(body.toString('utf8')) as unknown, failed: false };
  } catch {
    return { value: null, failed: true };
  }
}

/** Route one parsed request to its surface. */
function dispatch(forge: Forge, req: Req): Res {
  const path = req.pathname;
  if (path === '/healthz') return json(200, { ok: true });
  if (!isAuthenticated(req)) return err(401, 'Requires authentication');
  if (path === '/api/graphql') {
    if (req.method !== 'POST') return err(405, 'Method Not Allowed');
    return handleGraphql(forge, req);
  }
  if (path === '/mcp') {
    if (req.method !== 'POST') return err(405, 'Method Not Allowed');
    return handleMcp(forge, req);
  }
  if (path === '/api/v3' || path.startsWith('/api/v3/')) return handleRest(forge, req);
  const git = /^\/([^/]+)\/([^/]+?)\.git(?:\/(.*))?$/.exec(path);
  if (git !== null) {
    const owner = decodeURIComponent(git[1] as string);
    const repo = decodeURIComponent(git[2] as string);
    const rest = git[3] ?? '';
    return handleGitHttp(forge, req, owner, repo, rest);
  }
  return err(404, 'Not Found');
}

/** Write a Res onto the socket. */
function writeResponse(res: ServerResponse, out: Res): void {
  res.statusCode = out.status;
  if (out.contentType !== undefined) res.setHeader('Content-Type', out.contentType);
  if (out.headers !== undefined) {
    for (const [name, value] of Object.entries(out.headers)) res.setHeader(name, value);
  }
  if (out.body === undefined) {
    res.end();
    return;
  }
  res.end(out.body);
}

/** Build the forge server. The lifecycle in main.ts binds it to an ephemeral port. */
export function createApp(): Server {
  const forge = new Forge();
  const server = createServer((req, res) => {
    void (async () => {
      res.setHeader('X-GitHub-Request-Id', newRequestId());
      try {
        const rawUrl = req.url ?? '/';
        const host = req.headers.host ?? '127.0.0.1';
        const { pathname, segments, query } = splitTarget(rawUrl);
        // Most requests carry no body at all: do not touch the stream for those.
        const body = hasBody(req) ? await collectBody(req) : Buffer.alloc(0);
        if (body === null) {
          writeResponse(res, err(413, 'Request body too large'));
          return;
        }
        // Git bodies are binary pkt-lines and packs: no handler on a git route ever reads
        // req.json, so skip the decode-and-parse entirely there. Every other surface keeps
        // the exact rule — any non-empty body that is not valid JSON is a 400.
        const gitPath = /^\/[^/]+\/[^/]+?\.git(?:\/.*)?$/.test(pathname);
        const decoded = decodeJsonBody(body, gitPath);
        const parsedJson = decoded.value;
        const jsonParseError = decoded.failed;
        const request: Req = {
          method: (req.method ?? 'GET').toUpperCase(),
          pathname,
          segments,
          query,
          headers: req.headers,
          body,
          json: parsedJson,
          jsonParseError,
          host,
        };
        writeResponse(res, dispatch(forge, request));
      } catch {
        // One bad request must never take the server down.
        try {
          writeResponse(res, err(500, 'Internal Server Error'));
        } catch {
          // The socket is already gone; there is nothing left to answer.
        }
      }
    })();
  });
  // No Date header: the trial replays must be byte-identical, and the wall clock is not.
  // (sendDate is a real http.Server property; the @types/node declarations omit it.)
  (server as unknown as { sendDate: boolean }).sendDate = false;
  return server;
}
