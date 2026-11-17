// GitHub REST v3: repositories, branches, commits, contents, labels, issues, comments and pull
// requests — all on the one shared store, so GraphQL and MCP read back exactly what REST wrote.
//
// Routes served here, all under /api/v3 (bearer auth is enforced by the router, not here):
//   GET  /                                  GET  /meta                      GET  /user
//   POST /user/repos                        GET  /user/repos
//   GET/PATCH/DELETE /repos/{o}/{r}
//   GET  /repos/{o}/{r}/branches            GET  /repos/{o}/{r}/branches/{b}
//   GET  /repos/{o}/{r}/commits[?sha=]
//   POST /repos/{o}/{r}/git/refs            GET  /repos/{o}/{r}/git/refs/heads/{b}
//   PUT/GET/DELETE /repos/{o}/{r}/contents/{path}
//   POST/GET /repos/{o}/{r}/labels
//   POST/GET /repos/{o}/{r}/issues          GET/PATCH /repos/{o}/{r}/issues/{n}
//   POST/GET /repos/{o}/{r}/issues/{n}/comments
//   POST/GET /repos/{o}/{r}/pulls           GET  /repos/{o}/{r}/pulls/{n}
//   GET  /repos/{o}/{r}/pulls/{n}/files     PUT  /repos/{o}/{r}/pulls/{n}/merge
//
// Every ref change goes through `repo.setRef`, which bumps the repository version and so
// invalidates the git layer's cached fetch answers.

import {
  FIXED_ISO,
  buildTree,
  countLines,
  lineDiff,
  parseCommit,
  parseTree,
  type Forge,
  type Issue,
  type Label,
  type PullRequest,
  type Repo,
} from './store.ts';
import { bodyObject, err, json, noContent, paginate, queryInt, type Req, type Res } from './http.ts';

// ---------------------------------------------------------------------------
// Shared domain operations (REST, GraphQL and MCP all call these)
// ---------------------------------------------------------------------------

export function createIssue(
  forge: Forge,
  repo: Repo,
  input: { title: string; body?: string; labels?: readonly string[] },
): { ok: true; issue: Issue } | { ok: false; status: number; message: string } {
  if (typeof input.title !== 'string' || input.title === '') return { ok: false, status: 422, message: 'title is required' };
  const labels: Label[] = [];
  for (const name of input.labels ?? []) {
    const label = repo.labels.get(name);
    if (!label) return { ok: false, status: 422, message: `Label "${name}" does not exist` };
    labels.push(label);
  }
  const issue: Issue = {
    id: forge.nextGlobalId(),
    number: repo.nextNumber,
    title: input.title,
    body: input.body ?? '',
    state: 'open',
    labels,
    comments: [],
    seq: repo.nextSeq(),
  };
  repo.nextNumber += 1;
  repo.issues.set(issue.number, issue);
  return { ok: true, issue };
}

export function closeIssue(_repo: Repo, issue: Issue): void {
  issue.state = 'closed';
}

export function createPull(
  forge: Forge,
  repo: Repo,
  input: { title: string; body?: string; head: string; base: string },
): { ok: true; pull: PullRequest } | { ok: false; status: number; message: string } {
  if (typeof input.title !== 'string' || input.title === '') return { ok: false, status: 422, message: 'title is required' };
  if (typeof input.head !== 'string' || input.head === '') return { ok: false, status: 422, message: 'head is required' };
  const base = typeof input.base === 'string' && input.base !== '' ? input.base : repo.defaultBranch;
  if (repo.branchTip(input.head) === undefined) return { ok: false, status: 422, message: `head branch "${input.head}" does not exist` };
  if (repo.branchTip(base) === undefined) return { ok: false, status: 422, message: `base branch "${base}" does not exist` };
  const pull: PullRequest = {
    id: forge.nextGlobalId(),
    number: repo.nextNumber,
    title: input.title,
    body: input.body ?? '',
    state: 'open',
    merged: false,
    mergeCommitSha: null,
    mergedAt: null,
    headRef: input.head,
    baseRef: base,
    seq: repo.nextSeq(),
  };
  repo.nextNumber += 1;
  repo.pulls.set(pull.number, pull);
  return { ok: true, pull };
}

export interface PullFile {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
}

/** The files a pull request changes, between its merge-base and its head. */
export function pullFiles(repo: Repo, pull: PullRequest): PullFile[] {
  const headTip = repo.branchTip(pull.headRef);
  if (headTip === undefined) return [];
  const headCommit = repo.commitOf(headTip);
  if (!headCommit) return [];
  const baseTip = repo.branchTip(pull.baseRef);
  let baseTree: string | undefined;
  if (baseTip !== undefined) {
    const base = repo.mergeBase(baseTip, headTip);
    const baseCommit = base === null ? null : repo.commitOf(base);
    baseTree = baseCommit?.view.tree;
  }
  const before = repo.flattenTree(baseTree);
  const after = repo.flattenTree(headCommit.view.tree);
  const files: PullFile[] = [];
  for (const path of [...new Set([...before.keys(), ...after.keys()])].sort()) {
    const oldEntry = before.get(path);
    const newEntry = after.get(path);
    if (oldEntry && !newEntry) {
      const removed = countLines(repo.objects.get(oldEntry.oid)?.data ?? Buffer.alloc(0));
      files.push({ filename: path, status: 'removed', additions: 0, deletions: removed, changes: removed });
    } else if (!oldEntry && newEntry) {
      const added = countLines(repo.objects.get(newEntry.oid)?.data ?? Buffer.alloc(0));
      files.push({ filename: path, status: 'added', additions: added, deletions: 0, changes: added });
    } else if (oldEntry && newEntry && oldEntry.oid !== newEntry.oid) {
      const { additions, deletions } = lineDiff(
        repo.objects.get(oldEntry.oid)?.data ?? null,
        repo.objects.get(newEntry.oid)?.data ?? null,
      );
      files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  return files;
}

/**
 * Whether the pull request conflicts with its base: some path the head changed since the
 * merge-base that the base also changed, to different bytes.
 */
export function pullConflicts(repo: Repo, pull: PullRequest): boolean {
  const headTip = repo.branchTip(pull.headRef);
  const baseTip = repo.branchTip(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) return false;
  const headCommit = repo.commitOf(headTip);
  const baseTipCommit = repo.commitOf(baseTip);
  if (!headCommit || !baseTipCommit) return false;
  const base = repo.mergeBase(baseTip, headTip);
  const baseCommit = base === null ? null : repo.commitOf(base);
  const ancestorFiles = repo.flattenTree(baseCommit?.view.tree);
  const headFiles = repo.flattenTree(headCommit.view.tree);
  const baseFiles = repo.flattenTree(baseTipCommit.view.tree);
  const paths = new Set([...ancestorFiles.keys(), ...headFiles.keys(), ...baseFiles.keys()]);
  for (const path of paths) {
    const ancestor = ancestorFiles.get(path)?.oid ?? null;
    const head = headFiles.get(path)?.oid ?? null;
    const baseVersion = baseFiles.get(path)?.oid ?? null;
    if (head !== ancestor && baseVersion !== ancestor && head !== baseVersion) return true;
  }
  return false;
}

/** Squash-merge a pull request onto its base branch. */
export function mergePull(
  repo: Repo,
  pull: PullRequest,
): { ok: true; sha: string } | { ok: false; status: number; message: string } {
  if (pull.merged) return { ok: false, status: 405, message: 'Pull request has already been merged' };
  if (pull.state !== 'open') return { ok: false, status: 405, message: 'Pull request is closed' };
  const headTip = repo.branchTip(pull.headRef);
  const baseTip = repo.branchTip(pull.baseRef);
  if (headTip === undefined || baseTip === undefined) return { ok: false, status: 422, message: 'head or base branch does not exist' };
  if (pullConflicts(repo, pull)) return { ok: false, status: 405, message: 'Merge conflict: the head branch conflicts with the base branch' };
  const base = repo.mergeBase(baseTip, headTip);
  const baseCommit = base === null ? null : repo.commitOf(base);
  const headCommit = repo.commitOf(headTip);
  const baseTipCommit = repo.commitOf(baseTip);
  if (!headCommit || !baseTipCommit) return { ok: false, status: 422, message: 'head or base commit not found' };
  // The merged tree: the base tip's tree with the head's changes applied on top.
  const ancestorFiles = repo.flattenTree(baseCommit?.view.tree);
  const headFiles = repo.flattenTree(headCommit.view.tree);
  const merged = repo.flattenTree(baseTipCommit.view.tree);
  for (const path of new Set([...ancestorFiles.keys(), ...headFiles.keys()])) {
    const ancestor = ancestorFiles.get(path)?.oid ?? null;
    const head = headFiles.get(path);
    if ((head?.oid ?? null) === ancestor) continue;
    if (head === undefined) merged.delete(path);
    else merged.set(path, { oid: head.oid, mode: head.mode });
  }
  const sorted = [...merged.entries()].sort(([a], [b]) => (a < b ? -1 : 1));
  let tree: string | undefined;
  for (const [path, entry] of sorted) tree = repo.updateTree(tree, path, entry.oid);
  if (tree === undefined) tree = repo.putObject('tree', buildTree([]));
  const sha = repo.createCommit(tree, [baseTip], `${pull.title} (#${pull.number})`);
  repo.setRef(`refs/heads/${pull.baseRef}`, sha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergeCommitSha = sha;
  pull.mergedAt = FIXED_ISO;
  return { ok: true, sha };
}

// ---------------------------------------------------------------------------
// JSON shapes (key order is wire-visible; keep it exactly as written)
// ---------------------------------------------------------------------------

function userJson(forge: Forge): Record<string, unknown> {
  return { login: forge.viewer.login, id: forge.viewer.id, type: 'User', name: forge.viewer.name, email: forge.viewer.email };
}

export function repoJson(forge: Forge, repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    name: repo.name,
    full_name: repo.fullName,
    owner: { login: repo.owner, id: forge.viewer.id, type: 'User' },
    private: repo.privateFlag,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    fork: false,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
  };
}

function labelJson(label: Label): Record<string, unknown> {
  return { id: label.id, name: label.name, color: label.color };
}

export function issueJson(forge: Forge, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map(labelJson),
    user: userJson(forge),
    comments: issue.comments.length,
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
  };
}

export function commentJson(forge: Forge, comment: { id: number; body: string }): Record<string, unknown> {
  return { id: comment.id, body: comment.body, user: userJson(forge), created_at: FIXED_ISO, updated_at: FIXED_ISO };
}

export function pullJson(forge: Forge, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const headSha = repo.branchTip(pull.headRef) ?? '';
  const baseSha = repo.branchTip(pull.baseRef) ?? '';
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    mergeable: pull.state === 'open' ? !pullConflicts(repo, pull) : null,
    merge_commit_sha: pull.mergeCommitSha,
    merged_at: pull.mergedAt,
    head: { ref: pull.headRef, sha: headSha, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: baseSha, label: `${repo.owner}:${pull.baseRef}` },
    user: userJson(forge),
    created_at: FIXED_ISO,
    updated_at: FIXED_ISO,
  };
}

function commitJson(repo: Repo, oid: string): Record<string, unknown> | null {
  const commit = repo.commitOf(oid);
  if (!commit) return null;
  const parseIdentity = (line: string): { name: string; email: string; date: string } => {
    const match = /^(.+?) <(.*?)> (\d+) ([+-]\d{4})$/.exec(line);
    if (!match) return { name: line, email: '', date: FIXED_ISO };
    const date = new Date(Number(match[3]) * 1000).toISOString();
    return { name: match[1] as string, email: match[2] as string, date };
  };
  return {
    sha: commit.oid,
    commit: {
      // Git stores the message with a trailing newline; the API presents it without one.
      message: commit.view.message.replace(/\n+$/, ''),
      author: parseIdentity(commit.view.author),
      committer: parseIdentity(commit.view.committer),
    },
    parents: commit.view.parents.map((sha) => ({ sha })),
    author: null,
    committer: null,
  };
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

function putContents(forge: Forge, repo: Repo, path: string, req: Req): Res {
  void forge;
  const body = bodyObject(req);
  if (body === null) return err(422, 'a JSON body is required');
  const message = typeof body.message === 'string' ? body.message : `Update ${path}`;
  const content = body.content;
  if (typeof content !== 'string') return err(422, 'content is required');
  let bytes: Buffer;
  try {
    bytes = Buffer.from(content.replace(/\s+/g, ''), 'base64');
  } catch {
    return err(422, 'content is not valid base64');
  }
  const branch = typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
  let tip = repo.branchTip(branch);
  if (tip === undefined && branch !== repo.defaultBranch && repo.defaultTip() !== undefined) {
    // A contents write on a branch that does not exist yet starts it at the default tip.
    tip = repo.defaultTip();
  }
  const tipCommit = tip === undefined ? null : repo.commitOf(tip);
  const existing = tipCommit === null ? null : repo.entryAt(tipCommit.view.tree, path);
  const sha = typeof body.sha === 'string' ? body.sha : undefined;
  if (sha !== undefined && existing !== null && sha.toLowerCase() !== existing.oid.toLowerCase()) {
    return err(409, 'sha does not match the current file');
  }
  if (sha !== undefined && existing === null && tipCommit !== null) {
    // An update naming a file that is not there: GitHub answers 422, but the suite never sends
    // one; treat it as a create so a stray sha cannot wedge a write.
  }
  const blobOid = repo.putObject('blob', bytes);
  const tree = repo.updateTree(tipCommit?.view.tree, path, blobOid);
  const commitSha = repo.createCommit(tree, tip === undefined ? [] : [tip], message);
  repo.setRef(`refs/heads/${branch}`, commitSha);
  return json(existing === null ? 201 : 200, {
    content: { name: path.split('/').pop() ?? path, path, sha: blobOid, size: bytes.length, encoding: 'base64', type: 'file' },
    commit: { sha: commitSha },
  });
}

function listDirectory(repo: Repo, treeOid: string, dirPath: string): Array<Record<string, unknown>> {
  const object = repo.objects.get(treeOid);
  if (!object || object.type !== 'tree') return [];
  return parseTree(object.data).map((entry) => ({
    type: entry.mode === '40000' ? 'dir' : 'file',
    name: entry.name,
    path: dirPath === '' ? entry.name : `${dirPath}/${entry.name}`,
    sha: entry.oid,
    size: entry.mode === '40000' ? 0 : (repo.objects.get(entry.oid)?.data.length ?? 0),
  }));
}

function getContents(repo: Repo, path: string, req: Req): Res {
  const tip = repo.resolveRef(req.query.get('ref') ?? undefined);
  if (tip === undefined) return err(404, 'Not Found');
  const commit = repo.commitOf(tip);
  if (!commit) return err(404, 'Not Found');
  if (path === '') return json(200, listDirectory(repo, commit.view.tree, ''));
  const entry = repo.entryAt(commit.view.tree, path);
  if (entry === null) return err(404, 'Not Found');
  if (entry.mode === '40000') return json(200, listDirectory(repo, entry.oid, path));
  const blob = repo.objects.get(entry.oid);
  if (!blob) return err(404, 'Not Found');
  return json(200, {
    type: 'file',
    encoding: 'base64',
    size: blob.data.length,
    name: path.split('/').pop() ?? path,
    path,
    content: blob.data.toString('base64'),
    sha: entry.oid,
  });
}

function deleteContents(repo: Repo, path: string, req: Req): Res {
  const body = bodyObject(req);
  const message = body !== null && typeof body.message === 'string' ? body.message : `Delete ${path}`;
  const branch = body !== null && typeof body.branch === 'string' && body.branch !== '' ? body.branch : repo.defaultBranch;
  const tip = repo.branchTip(branch);
  if (tip === undefined) return err(404, 'Not Found');
  const commit = repo.commitOf(tip);
  if (!commit) return err(404, 'Not Found');
  const existing = repo.entryAt(commit.view.tree, path);
  if (existing === null) return err(404, 'Not Found');
  const sha = body !== null && typeof body.sha === 'string' ? body.sha : undefined;
  if (sha !== undefined && sha.toLowerCase() !== existing.oid.toLowerCase()) {
    return err(409, 'sha does not match the current file');
  }
  const tree = repo.updateTree(commit.view.tree, path, null);
  const commitSha = repo.createCommit(tree, [tip], message);
  repo.setRef(`refs/heads/${branch}`, commitSha);
  return json(200, { commit: { sha: commitSha } });
}

// ---------------------------------------------------------------------------
// The router
// ---------------------------------------------------------------------------

/** Handle one /api/v3 request. `segments` are the path parts after `/api/v3`. */
export function handleRest(forge: Forge, req: Req): Res {
  const segments = req.segments.slice(2); // drop 'api', 'v3'
  const method = req.method;

  if ((method === 'POST' || method === 'PUT' || method === 'PATCH' || method === 'DELETE') && req.jsonParseError) {
    return err(400, 'Malformed JSON body');
  }

  // API root and meta.
  if (segments.length === 0) {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, {
      current_user_url: '/api/v3/user',
      repository_url: '/api/v3/repos/{owner}/{repo}',
      user_repositories_url: '/api/v3/user/repos',
    }, { 'x-oauth-scopes': 'repo, read:org' });
  }
  if (segments.length === 1 && segments[0] === 'meta') {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, { installed_version: '1.0.0', verifiable_password_authentication: true });
  }
  if (segments.length === 1 && segments[0] === 'user') {
    if (method !== 'GET') return err(404, 'Not Found');
    return json(200, userJson(forge));
  }

  // /user/repos
  if (segments[0] === 'user' && segments[1] === 'repos' && segments.length === 2) {
    if (method === 'POST') {
      const body = bodyObject(req);
      const name = body?.name;
      if (typeof name !== 'string' || name === '' || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/.test(name)) {
        return err(422, 'name is required');
      }
      if (forge.getRepo(forge.viewer.login, name) !== undefined) {
        return err(422, 'Repository already exists');
      }
      const repo = forge.createRepo(
        name,
        typeof body?.description === 'string' ? body.description : '',
        body?.private === true,
      );
      return json(201, repoJson(forge, repo));
    }
    if (method === 'GET') {
      const repos = forge.viewerRepos();
      const { page, link } = paginate(repos, req, req.pathname);
      return json(200, page.map((repo) => repoJson(forge, repo)), link === null ? undefined : { link });
    }
    return err(404, 'Not Found');
  }

  // /repos/{owner}/{repo}[...]
  if (segments[0] === 'repos' && segments.length >= 3) {
    const owner = segments[1] as string;
    const name = segments[2] as string;
    const repo = forge.getRepo(owner, name);
    const rest = segments.slice(3);
    if (repo === undefined) return err(404, 'Not Found');

    if (rest.length === 0) {
      if (method === 'GET') return json(200, repoJson(forge, repo));
      if (method === 'PATCH') {
        const body = bodyObject(req);
        if (body !== null) {
          if (typeof body.description === 'string') repo.description = body.description;
          if (typeof body.private === 'boolean') repo.privateFlag = body.private;
        }
        return json(200, repoJson(forge, repo));
      }
      if (method === 'DELETE') {
        forge.deleteRepo(owner, name);
        return noContent();
      }
      return err(404, 'Not Found');
    }

    if (rest[0] === 'branches') {
      if (rest.length === 1 && method === 'GET') {
        const branches: Array<Record<string, unknown>> = [];
        for (const ref of [...repo.refs.keys()].sort()) {
          if (!ref.startsWith('refs/heads/')) continue;
          branches.push({ name: ref.slice('refs/heads/'.length), commit: { sha: repo.refs.get(ref) } });
        }
        return json(200, branches);
      }
      if (rest.length === 2 && method === 'GET') {
        const branch = rest[1] as string;
        const tip = repo.branchTip(branch);
        if (tip === undefined) return err(404, 'Not Found');
        return json(200, { name: branch, commit: { sha: tip } });
      }
      return err(404, 'Not Found');
    }

    if (rest[0] === 'commits' && rest.length === 1 && method === 'GET') {
      const tip = repo.resolveRef(req.query.get('sha') ?? undefined);
      const commits: Array<Record<string, unknown>> = [];
      for (const { oid } of repo.commitHistory(tip)) {
        const entry = commitJson(repo, oid);
        if (entry) commits.push(entry);
      }
      const perPage = queryInt(req.query, 'per_page', 30, 1, 100);
      const page = queryInt(req.query, 'page', 1, 1, 1_000_000);
      return json(200, commits.slice((page - 1) * perPage, page * perPage));
    }

    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (rest.length === 2 && method === 'POST') {
        const body = bodyObject(req);
        const ref = body?.ref;
        const sha = body?.sha;
        if (typeof ref !== 'string' || !ref.startsWith('refs/')) return err(422, 'ref is required');
        if (typeof sha !== 'string' || repo.objects.get(sha) === undefined) return err(422, 'sha does not exist');
        if (repo.refs.has(ref)) return err(422, 'Reference already exists');
        repo.setRef(ref, sha);
        return json(201, { ref, object: { sha } });
      }
      if (rest.length >= 3 && method === 'GET') {
        const ref = `refs/${rest.slice(2).join('/')}`;
        const tip = repo.refs.get(ref);
        if (tip === undefined) return err(404, 'Not Found');
        return json(200, { ref, object: { sha: tip } });
      }
      return err(404, 'Not Found');
    }

    if (rest[0] === 'contents') {
      const path = rest.slice(1).join('/');
      if (method === 'PUT') return putContents(forge, repo, path, req);
      if (method === 'GET') return getContents(repo, path, req);
      if (method === 'DELETE') return deleteContents(repo, path, req);
      return err(404, 'Not Found');
    }

    if (rest[0] === 'labels') {
      if (rest.length === 1 && method === 'POST') {
        const body = bodyObject(req);
        const name = body?.name;
        if (typeof name !== 'string' || name === '') return err(422, 'name is required');
        const existing = repo.labels.get(name);
        if (existing) return json(201, labelJson(existing));
        const label: Label = { id: forge.nextGlobalId(), name, color: typeof body?.color === 'string' ? body.color : 'ededed' };
        repo.labels.set(name, label);
        return json(201, labelJson(label));
      }
      if (rest.length === 1 && method === 'GET') {
        return json(200, [...repo.labels.values()].map(labelJson));
      }
      return err(404, 'Not Found');
    }

    if (rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const body = bodyObject(req);
          if (body === null) return err(422, 'a JSON body is required');
          const labels = Array.isArray(body.labels) ? body.labels.filter((entry): entry is string => typeof entry === 'string') : [];
          const created = createIssue(forge, repo, { title: typeof body.title === 'string' ? body.title : '', body: typeof body.body === 'string' ? body.body : '', labels });
          if (!created.ok) return err(created.status, created.message);
          return json(201, issueJson(forge, repo, created.issue));
        }
        if (method === 'GET') {
          const state = req.query.get('state') ?? 'open';
          const direction = req.query.get('direction') ?? 'desc';
          let issues = [...repo.issues.values()];
          if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
          else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
          issues.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
          const { page, link } = paginate(issues, req, req.pathname);
          return json(200, page.map((issue) => issueJson(forge, repo, issue)), link === null ? undefined : { link });
        }
        return err(404, 'Not Found');
      }
      const number = Number(rest[1]);
      const issue = Number.isInteger(number) ? repo.issues.get(number) : undefined;
      if (issue === undefined) return err(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return json(200, issueJson(forge, repo, issue));
        if (method === 'PATCH') {
          const body = bodyObject(req);
          if (body !== null) {
            if (body.state === 'open' || body.state === 'closed') issue.state = body.state;
            if (typeof body.title === 'string') issue.title = body.title;
            if (typeof body.body === 'string') issue.body = body.body;
          }
          return json(200, issueJson(forge, repo, issue));
        }
        return err(404, 'Not Found');
      }
      if (rest[2] === 'comments' && rest.length === 3) {
        if (method === 'POST') {
          const body = bodyObject(req);
          if (body === null || typeof body.body !== 'string' || body.body === '') return err(422, 'body is required');
          const comment = { id: forge.nextComment(), body: body.body, userLogin: forge.viewer.login };
          issue.comments.push(comment);
          return json(201, commentJson(forge, comment));
        }
        if (method === 'GET') {
          return json(200, issue.comments.map((comment) => commentJson(forge, comment)));
        }
        return err(404, 'Not Found');
      }
      return err(404, 'Not Found');
    }

    if (rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'POST') {
          const body = bodyObject(req);
          if (body === null) return err(422, 'a JSON body is required');
          const created = createPull(forge, repo, {
            title: typeof body.title === 'string' ? body.title : '',
            body: typeof body.body === 'string' ? body.body : '',
            head: typeof body.head === 'string' ? body.head : '',
            base: typeof body.base === 'string' ? body.base : repo.defaultBranch,
          });
          if (!created.ok) return err(created.status, created.message);
          return json(201, pullJson(forge, repo, created.pull));
        }
        if (method === 'GET') {
          const state = req.query.get('state') ?? 'open';
          let pulls = [...repo.pulls.values()];
          if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
          else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
          pulls.sort((a, b) => b.seq - a.seq);
          const { page, link } = paginate(pulls, req, req.pathname);
          return json(200, page.map((pull) => pullJson(forge, repo, pull)), link === null ? undefined : { link });
        }
        return err(404, 'Not Found');
      }
      const number = Number(rest[1]);
      const pull = Number.isInteger(number) ? repo.pulls.get(number) : undefined;
      if (pull === undefined) return err(404, 'Not Found');
      if (rest.length === 2) {
        if (method === 'GET') return json(200, pullJson(forge, repo, pull));
        return err(404, 'Not Found');
      }
      if (rest[2] === 'files' && rest.length === 3 && method === 'GET') {
        return json(200, pullFiles(repo, pull));
      }
      if (rest[2] === 'merge' && rest.length === 3 && method === 'PUT') {
        const merged = mergePull(repo, pull);
        if (!merged.ok) return err(merged.status, merged.message);
        return json(200, { sha: merged.sha, merged: true, message: `Pull Request successfully merged` });
      }
      return err(404, 'Not Found');
    }
  }

  return err(404, 'Not Found');
}
