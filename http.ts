// Shared HTTP plumbing for the forge's surfaces: the request and response shapes the routers
// pass around, and the JSON helpers that keep every reply `application/json` with a message
// on errors.
//
// Hot-path note: the request's query string is parsed only when the raw URL actually carries
// one — most workload requests do not.

import type { IncomingHttpHeaders } from 'node:http';

export interface Req {
  method: string;
  /** The decoded pathname, e.g. `/api/v3/repos/adaiversen/amber-thistle`. */
  pathname: string;
  /** The pathname split on `/`, without empty segments. */
  segments: string[];
  query: URLSearchParams;
  headers: IncomingHttpHeaders;
  /** The raw request body. */
  body: Buffer;
  /** The parsed JSON body, when the body parsed as JSON. */
  json: unknown;
  /** True when a body was present but did not parse as JSON. */
  jsonParseError: boolean;
  /** The request's Host header, for absolute pagination URLs. */
  host: string;
}

export interface Res {
  status: number;
  /** Response body bytes (a string is sent as UTF-8). */
  body?: string | Buffer;
  contentType?: string;
  headers?: Record<string, string>;
}

let requestSeq = 0;

/** A request id: deterministic, non-empty, unique per request in this process. */
export function newRequestId(): string {
  requestSeq += 1;
  return `forge-${requestSeq.toString(36)}`;
}

/** A JSON reply. */
export function json(status: number, value: unknown, headers?: Record<string, string>): Res {
  return { status, body: JSON.stringify(value), contentType: 'application/json', headers };
}

/** A JSON error reply: `{message}`. */
export function err(status: number, message: string): Res {
  return json(status, { message });
}

/** A 204 with no body. */
export function noContent(): Res {
  return { status: 204 };
}

/** The parsed JSON body as a record, or null when it is not one. */
export function bodyObject(req: Req): Record<string, unknown> | null {
  if (req.json !== null && typeof req.json === 'object' && !Array.isArray(req.json)) {
    return req.json as Record<string, unknown>;
  }
  return null;
}

/** A query integer with a default, clamped to a range. */
export function queryInt(query: URLSearchParams, name: string, fallback: number, min: number, max: number): number {
  const raw = query.get(name);
  if (raw === null) return fallback;
  const value = Number.parseInt(raw, 10);
  if (!Number.isFinite(value)) return fallback;
  return Math.min(max, Math.max(min, value));
}

/** Paginate a list; returns the page and the Link header value (null when there is one page). */
export function paginate<T>(items: readonly T[], req: Req, path: string): { page: T[]; link: string | null } {
  const perPage = queryInt(req.query, 'per_page', 30, 1, 100);
  const page = queryInt(req.query, 'page', 1, 1, 1_000_000);
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const urlFor = (p: number): string => {
    const params = new URLSearchParams(req.query);
    params.set('page', String(p));
    params.set('per_page', String(perPage));
    return `http://${req.host}${path}?${params.toString()}`;
  };
  const links: string[] = [];
  if (page < totalPages) links.push(`<${urlFor(page + 1)}>; rel="next"`);
  if (page > 1) links.push(`<${urlFor(page - 1)}>; rel="prev"`);
  links.push(`<${urlFor(1)}>; rel="first"`);
  links.push(`<${urlFor(totalPages)}>; rel="last"`);
  return { page: slice, link: links.length > 0 ? links.join(', ') : null };
}
