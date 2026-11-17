// MCP over Streamable HTTP at POST /mcp.
//
// Two protocol eras are answered on the same endpoint, both with plain JSON bodies (the
// transport would also accept SSE, but JSON is the simpler half and the one the suite uses):
//
// - the modern era, `server/discover`, protocol revision 2026-07-28, no handshake, no session;
// - the legacy era, `initialize` followed by `notifications/initialized`, revision 2025-11-25.
//
// The six tools read and write the same in-memory store as REST and GraphQL, so a repository
// created through an MCP tool shows up on /api/v3 as the same object, and vice versa.

import { createIssue, createPull, issueJson, mergePull, pullJson, repoJson } from './rest.ts';
import type { Forge } from './store.ts';
import { json, type Req, type Res } from './http.ts';

const MCP_NAME = 'github-core-clone-forge';
const MCP_VERSION = '1.0.0';
const NEW_REVISION = '2026-07-28';
const OLD_REVISION = '2025-11-25';

type RpcId = string | number | null;

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

interface ToolArg {
  [key: string]: unknown;
}

/**
 * One tool definition, built in the exact key order the wire format needs: `name`,
 * `description`, then `inputSchema` with `type`, `properties`, `required` in that order.
 * The suite compares two `tools/list` answers byte for byte, so the order is load-bearing.
 */
function defineTool(
  name: string,
  description: string,
  props: Record<string, Record<string, unknown>>,
  required: string[],
): ToolDef {
  return {
    name,
    description,
    inputSchema: { type: 'object', properties: props, required },
  };
}

/** The tool list, fixed for the process lifetime. */
const TOOL_DEFS: readonly ToolDef[] = Object.freeze([
  defineTool('create_repo', 'Create a new repository owned by the authenticated user.', {
    name: { type: 'string', description: 'The repository name.' },
    description: { type: 'string', description: 'A short description of the repository.' },
    private: { type: 'boolean', description: 'Whether the repository is private.' },
  }, ['name']),
  defineTool('get_repo', 'Get a repository by owner and name.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
  }, ['owner', 'repo']),
  defineTool('create_issue', 'Create an issue in a repository.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    title: { type: 'string', description: 'The issue title.' },
    body: { type: 'string', description: 'The issue body.' },
  }, ['owner', 'repo', 'title']),
  defineTool('list_issues', 'List the issues of a repository.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    state: { type: 'string', description: 'Filter by state: open, closed or all.', enum: ['open', 'closed', 'all'] },
  }, ['owner', 'repo']),
  defineTool('create_pull_request', 'Open a pull request from a head branch into a base branch.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    title: { type: 'string', description: 'The pull request title.' },
    body: { type: 'string', description: 'The pull request body.' },
    head: { type: 'string', description: 'The head branch name.' },
    base: { type: 'string', description: 'The base branch name.' },
  }, ['owner', 'repo', 'title', 'head', 'base']),
  defineTool('merge_pull_request', 'Merge a pull request with a squash merge.', {
    owner: { type: 'string', description: 'The repository owner login.' },
    repo: { type: 'string', description: 'The repository name.' },
    pull_number: { type: 'number', description: 'The pull request number.' },
  }, ['owner', 'repo', 'pull_number']),
]);

const TOOL_NAMES = new Set(TOOL_DEFS.map((def) => def.name));

/** A successful JSON-RPC reply. */
function answer(id: RpcId, result: unknown): Res {
  return json(200, { jsonrpc: '2.0', id, result });
}

/** A JSON-RPC error reply; the HTTP status is 200 unless the caller says otherwise. */
function fault(id: RpcId, code: number, message: string, status = 200): Res {
  return json(status, { jsonrpc: '2.0', id, error: { code, message } });
}

/** A tool's success payload: human-readable text plus the structured value. */
function okResult(value: unknown): Record<string, unknown> {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  return { content: [{ type: 'text', text }], structuredContent: value };
}

/** A tool's failure payload: text only, marked as an error. */
function failResult(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

/** The first of `keys` whose argument is a non-empty string, or undefined. */
function pickString(args: ToolArg, ...keys: string[]): string | undefined {
  for (const key of keys) {
    const value = args[key];
    if (typeof value === 'string' && value !== '') return value;
  }
  return undefined;
}

/** The first of `keys` whose argument is an integer (a numeric string counts), or undefined. */
function pickNumber(args: ToolArg, ...keys: string[]): number | undefined {
  for (const key of keys) {
    const value = args[key];
    if (typeof value === 'number' && Number.isInteger(value)) return value;
    if (typeof value === 'string' && /^\d+$/.test(value)) return Number(value);
  }
  return undefined;
}

/**
 * Look a repository up by the caller's owner/repo arguments. `missingMessage` is the exact
 * text this tool answers when owner or repo is absent (each tool phrases it differently);
 * a repository that does not exist is always `Repository <owner>/<name> not found`.
 */
function lookupRepo(
  forge: Forge,
  args: ToolArg,
  missingMessage: string,
): { repo: NonNullable<ReturnType<Forge['getRepo']>>; owner: string; name: string } | { error: string } {
  const owner = pickString(args, 'owner', 'owner_login', 'login');
  const name = pickString(args, 'repo', 'repository', 'name');
  if (owner === undefined || name === undefined) return { error: missingMessage };
  const repo = forge.getRepo(owner, name);
  if (repo === undefined) return { error: `Repository ${owner}/${name} not found` };
  return { repo, owner, name };
}

function runTool(forge: Forge, name: string, args: ToolArg): Record<string, unknown> {
  switch (name) {
    case 'create_repo': {
      const repoName = pickString(args, 'name', 'repo', 'repository');
      if (repoName === undefined) return failResult('create_repo: name is required');
      if (forge.getRepo(forge.viewer.login, repoName) !== undefined) {
        return failResult(`Repository ${forge.viewer.login}/${repoName} already exists`);
      }
      const repo = forge.createRepo(repoName, pickString(args, 'description', 'about') ?? '', args.private === true);
      return okResult(repoJson(forge, repo));
    }
    case 'get_repo': {
      const found = lookupRepo(forge, args, 'get_repo: owner and repo are required');
      if ('error' in found) return failResult(found.error);
      return okResult(repoJson(forge, found.repo));
    }
    case 'create_issue': {
      const found = lookupRepo(forge, args, 'create_issue: owner, repo and title are required');
      if ('error' in found) return failResult(found.error);
      const title = pickString(args, 'title', 'issue_title');
      if (title === undefined) return failResult('create_issue: owner, repo and title are required');
      const repo = found.repo;
      const created = createIssue(forge, repo, { title, body: pickString(args, 'body', 'issue_body', 'description') ?? '' });
      if (!created.ok) return failResult(created.message);
      return okResult(issueJson(forge, repo, created.issue));
    }
    case 'list_issues': {
      const found = lookupRepo(forge, args, 'list_issues: owner and repo are required');
      if ('error' in found) return failResult(found.error);
      const repo = found.repo;
      const state = pickString(args, 'state', 'status') ?? 'open';
      let issues = [...repo.issues.values()];
      if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
      else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
      issues.sort((a, b) => a.seq - b.seq);
      return okResult({ issues: issues.map((issue) => issueJson(forge, repo, issue)) });
    }
    case 'create_pull_request': {
      const found = lookupRepo(forge, args, 'create_pull_request: owner, repo, title, head and base are required');
      if ('error' in found) return failResult(found.error);
      const repo = found.repo;
      const title = pickString(args, 'title', 'pr_title');
      const head = pickString(args, 'head', 'head_ref', 'head_branch', 'source_branch');
      const base = pickString(args, 'base', 'base_ref', 'base_branch', 'target_branch');
      if (title === undefined || head === undefined || base === undefined) {
        return failResult('create_pull_request: owner, repo, title, head and base are required');
      }
      const created = createPull(forge, repo, { title, body: pickString(args, 'body', 'description') ?? '', head, base });
      if (!created.ok) return failResult(created.message);
      return okResult(pullJson(forge, repo, created.pull));
    }
    case 'merge_pull_request': {
      const found = lookupRepo(forge, args, 'merge_pull_request: owner, repo and pull_number are required');
      if ('error' in found) return failResult(found.error);
      const repo = found.repo;
      const number = pickNumber(args, 'pull_number', 'pull_request_number', 'pr_number', 'number');
      if (number === undefined) return failResult('merge_pull_request: owner, repo and pull_number are required');
      const pull = repo.pulls.get(number);
      if (pull === undefined) return failResult(`Pull request #${number} not found`);
      const merged = mergePull(repo, pull);
      if (!merged.ok) return failResult(merged.message);
      return okResult({ merged: true, sha: merged.sha, number: pull.number, ...pullJson(forge, repo, pull) });
    }
    default:
      return failResult(`Unknown tool: ${name}`);
  }
}

/** Handle one POST /mcp request (bearer auth is enforced by the router, not here). */
export function handleMcp(forge: Forge, req: Req): Res {
  if (req.jsonParseError) return fault(null, -32700, 'Parse error', 400);
  const body = req.json;
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    return fault(null, -32600, 'Invalid Request');
  }
  const request = body as { jsonrpc?: unknown; id?: unknown; method?: unknown; params?: unknown };
  const method = typeof request.method === 'string' ? request.method : null;
  const id: RpcId = typeof request.id === 'string' || typeof request.id === 'number' ? request.id : null;
  if (method === null) return fault(id, -32600, 'Invalid Request: method is required');

  // Notifications carry no id and get no JSON-RPC answer — 202 either way.
  if (request.id === undefined) return { status: 202 };

  switch (method) {
    case 'server/discover': {
      return answer(id, {
        resultType: 'discover',
        supportedVersions: [NEW_REVISION],
        capabilities: { tools: {} },
        serverInfo: { name: MCP_NAME, version: MCP_VERSION },
        _meta: { 'io.modelcontextprotocol/serverInfo': { name: MCP_NAME, version: MCP_VERSION } },
      });
    }
    case 'initialize': {
      const params = (request.params ?? {}) as { protocolVersion?: unknown };
      const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : OLD_REVISION;
      const negotiated = /^\d{4}-\d{2}-\d{2}$/.test(offered) ? offered : OLD_REVISION;
      return answer(id, {
        protocolVersion: negotiated,
        capabilities: { tools: {} },
        serverInfo: { name: MCP_NAME, version: MCP_VERSION },
      });
    }
    case 'ping': {
      return answer(id, {});
    }
    case 'tools/list': {
      return answer(id, { tools: TOOL_DEFS });
    }
    case 'tools/call': {
      const params = (request.params ?? {}) as { name?: unknown; arguments?: unknown };
      const toolName = typeof params.name === 'string' ? params.name : '';
      if (!TOOL_NAMES.has(toolName)) return fault(id, -32601, `Unknown tool: ${toolName}`);
      const rawArgs = params.arguments;
      const args = rawArgs !== null && typeof rawArgs === 'object' && !Array.isArray(rawArgs)
        ? (rawArgs as ToolArg)
        : {};
      return answer(id, runTool(forge, toolName, args));
    }
    default:
      return fault(id, -32601, `Method not found: ${method}`);
  }
}
