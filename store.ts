// The forge's in-memory store.
//
// One process, no disk, no clock: git objects and refs per repository, plus the issues, pull
// requests, labels and comments that REST, GraphQL and MCP all read and write. Every timestamp
// the forge emits is fixed, so a replayed trial answers byte-identical bytes.
//
// Each repository carries a `version` counter that rises on every object or ref mutation. The
// git layer caches whole fetch answers keyed by that version — fifty concurrent clones of one
// unchanged history are answered from the cache — and any push, contents write or merge
// invalidates it at once. The counter is process-local and never leaves the server, so it
// cannot leak into a replayed export.

import { createHash } from 'node:crypto';
import { viewerUser } from './scenario.ts';

export const ZERO_OID = '0'.repeat(40);
export const DEFAULT_BRANCH = 'main';
/** Fixed timestamp every server-made commit carries (2023-11-14T22:13:20Z). */
export const FORGE_EPOCH_SEC = 1700000000;
/** Fixed ISO timestamp every REST/GraphQL created_at, updated_at and merged_at carries. */
export const FIXED_ISO = '2025-01-01T00:00:00.000Z';
/** Identity every server-made commit and tag carries. */
export const FORGE_IDENTITY = 'forge <forge@localhost>';

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

/** An object together with its id, so pack writers never re-hash what the store already knows. */
export interface NamedObject {
  readonly oid: string;
  readonly object: GitObject;
}

export interface TreeEntry {
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

export interface CommitView {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  readonly message: string;
}

const OID_PATTERN = /^[0-9a-f]{40}$/;

export function isObjectId(value: string): boolean {
  return OID_PATTERN.test(value);
}

/** git's object id: SHA-1 over `<type> <size>\0` followed by the object's bytes. */
export function hashObject(type: GitObjectType, data: Uint8Array): string {
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${data.length}\u0000`, 'utf8'));
  hash.update(data);
  return hash.digest('hex');
}

/** Parse a tree object into its entries, in stored order. */
export function parseTree(data: Uint8Array): TreeEntry[] {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < buffer.length) {
    const space = buffer.indexOf(0x20, offset);
    const nul = buffer.indexOf(0x00, space + 1);
    entries.push({
      mode: buffer.subarray(offset, space).toString('ascii'),
      name: buffer.subarray(space + 1, nul).toString('utf8'),
      oid: buffer.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

/** git's tree ordering: a directory sorts as though its name ended in `/`. */
function treeOrder(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

/** Build a tree object's bytes; entries are sorted the way git sorts them. */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const ordered = [...entries].sort(treeOrder);
  const chunks: Buffer[] = [];
  for (const entry of ordered) {
    chunks.push(Buffer.from(`${entry.mode} ${entry.name}\u0000`, 'utf8'));
    chunks.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(chunks);
}

/** Parse a commit object: headers above the blank line, message below it. */
export function parseCommit(data: Uint8Array): CommitView {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const blank = buffer.indexOf('\n\n');
  const headerEnd = blank < 0 ? buffer.length : blank;
  const message = blank < 0 ? '' : buffer.subarray(blank + 2).toString('utf8');
  const header = buffer.subarray(0, headerEnd).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of header.split('\n')) {
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') tree = value;
    else if (key === 'parent') parents.push(value);
    else if (key === 'author') author = value;
    else if (key === 'committer') committer = value;
  }
  return { tree, parents, author, committer, message };
}

/** Build a commit object's bytes with the forge's fixed identity and timestamp. */
export function buildCommitBytes(tree: string, parents: readonly string[], message: string): Buffer {
  const who = `${FORGE_IDENTITY} ${FORGE_EPOCH_SEC} +0000`;
  const lines = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${who}`, `committer ${who}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

/** Parse an annotated tag's target (object id and type). */
export function parseTagTarget(data: Uint8Array): { object: string; type: string } {
  const buffer = Buffer.isBuffer(data) ? data : Buffer.from(data);
  const blank = buffer.indexOf('\n\n');
  const headerEnd = blank < 0 ? buffer.length : blank;
  const header = buffer.subarray(0, headerEnd).toString('utf8');
  let object = '';
  let type = '';
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7);
    else if (line.startsWith('type ')) type = line.slice(5);
  }
  return { object, type };
}

// ---------------------------------------------------------------------------
// Issues, pulls, labels, comments
// ---------------------------------------------------------------------------

export interface Label {
  readonly id: number;
  readonly name: string;
  readonly color: string;
}

export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly userLogin: string;
}

export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  comments: IssueComment[];
  /** Creation sequence, for created-order sorts (timestamps are fixed). */
  readonly seq: number;
}

export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergeCommitSha: string | null;
  mergedAt: string | null;
  readonly headRef: string;
  readonly baseRef: string;
  readonly seq: number;
}

// ---------------------------------------------------------------------------
// Global (Relay) ids: opaque base64url of a typed payload, stable across reads.
// ---------------------------------------------------------------------------

function encodeB64url(text: string): string {
  return Buffer.from(text, 'utf8').toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function decodeB64url(text: string): string | null {
  try {
    return Buffer.from(text.replace(/-/g, '+').replace(/_/g, '/'), 'base64').toString('utf8');
  } catch {
    return null;
  }
}

export function repoGlobalId(repo: Repo): string {
  return encodeB64url(`Repository:${repo.id}`);
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return encodeB64url(`Issue:${repo.id}:${issue.number}`);
}

export function pullGlobalId(repo: Repo, pull: PullRequest): string {
  return encodeB64url(`PullRequest:${repo.id}:${pull.number}`);
}

export function commentGlobalId(repo: Repo, comment: IssueComment): string {
  return encodeB64url(`IssueComment:${repo.id}:${comment.id}`);
}

export type DecodedGlobalId =
  | { kind: 'Repository'; repoId: number }
  | { kind: 'Issue'; repoId: number; number: number }
  | { kind: 'PullRequest'; repoId: number; number: number }
  | { kind: 'IssueComment'; repoId: number; commentId: number };

export function decodeGlobalId(id: string): DecodedGlobalId | null {
  const text = decodeB64url(id);
  if (text === null) return null;
  const parts = text.split(':');
  const kind = parts[0];
  const first = Number(parts[1]);
  if (kind === 'Repository' && parts.length === 2) {
    return Number.isInteger(first) ? { kind: 'Repository', repoId: first } : null;
  }
  if ((kind === 'Issue' || kind === 'PullRequest') && parts.length === 3) {
    const number = Number(parts[2]);
    if (!Number.isInteger(first) || !Number.isInteger(number)) return null;
    return kind === 'Issue' ? { kind: 'Issue', repoId: first, number } : { kind: 'PullRequest', repoId: first, number };
  }
  if (kind === 'IssueComment' && parts.length === 3) {
    const commentId = Number(parts[2]);
    if (!Number.isInteger(first) || !Number.isInteger(commentId)) return null;
    return { kind: 'IssueComment', repoId: first, commentId };
  }
  return null;
}

/** Count the lines of a text blob (a trailing newline does not add a line). */
export function countLines(data: Buffer): number {
  if (data.length === 0) return 0;
  const text = data.toString('utf8');
  const lines = text.split('\n');
  if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
  return lines.length;
}

/** A simple multiset line diff: [added, removed] between two texts. */
export function lineDiff(oldData: Buffer | null, newData: Buffer | null): { additions: number; deletions: number } {
  const lineCounts = (data: Buffer | null): Map<string, number> => {
    const counts = new Map<string, number>();
    if (data === null || data.length === 0) return counts;
    const lines = data.toString('utf8').split('\n');
    if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
    for (const line of lines) counts.set(line, (counts.get(line) ?? 0) + 1);
    return counts;
  };
  const before = lineCounts(oldData);
  const after = lineCounts(newData);
  let additions = 0;
  let deletions = 0;
  for (const [line, count] of after) additions += Math.max(0, count - (before.get(line) ?? 0));
  for (const [line, count] of before) deletions += Math.max(0, count - (after.get(line) ?? 0));
  return { additions, deletions };
}

// ---------------------------------------------------------------------------
// Repository
// ---------------------------------------------------------------------------

export class Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  description: string;
  privateFlag: boolean;
  readonly defaultBranch: string;
  /** oid -> object. */
  readonly objects = new Map<string, GitObject>();
  /** full ref name -> oid. */
  readonly refs = new Map<string, string>();
  readonly labels = new Map<string, Label>();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, PullRequest>();
  /** Shared issue/PR numbering, GitHub-style. */
  nextNumber = 1;
  private orderCounter = 0;
  /** Rises on every object or ref change; the git layer's caches key on it. */
  version = 0;

  constructor(id: number, owner: string, name: string, description: string, privateFlag: boolean) {
    this.id = id;
    this.owner = owner;
    this.name = name;
    this.description = description;
    this.privateFlag = privateFlag;
    this.defaultBranch = DEFAULT_BRANCH;
  }

  get fullName(): string {
    return `${this.owner}/${this.name}`;
  }

  /** The next creation-sequence number (issues and pulls share one counter). */
  nextSeq(): number {
    this.orderCounter += 1;
    return this.orderCounter;
  }

  /** Record that the git state changed; cached fetch answers for this repo go stale. */
  bump(): void {
    this.version += 1;
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  /** Store one object, bumping the version only when it is actually new. */
  putObject(type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) {
      this.objects.set(oid, { type, data });
      this.bump();
    }
    return oid;
  }

  /** Store objects that already carry their ids (a received pack); one bump for the batch. */
  adoptObjects(objects: Iterable<NamedObject>): void {
    let changed = false;
    for (const { oid, object } of objects) {
      if (!this.objects.has(oid)) {
        this.objects.set(oid, object);
        changed = true;
      }
    }
    if (changed) this.bump();
  }

  refTip(ref: string): string | undefined {
    return this.refs.get(ref);
  }

  branchTip(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  defaultTip(): string | undefined {
    return this.refs.get(`refs/heads/${this.defaultBranch}`);
  }

  /** Move a ref, bumping the version only when the ref actually changes. */
  setRef(ref: string, oid: string): void {
    if (this.refs.get(ref) !== oid) {
      this.refs.set(ref, oid);
      this.bump();
    }
  }

  /** Delete a ref, bumping the version only when the ref existed. */
  deleteRef(ref: string): boolean {
    if (!this.refs.delete(ref)) return false;
    this.bump();
    return true;
  }

  /** Resolve a ref-ish: a branch name, a full ref, or a raw oid. */
  resolveRef(refish: string | undefined): string | undefined {
    if (refish === undefined || refish === '') return this.defaultTip();
    if (isObjectId(refish) && this.objects.has(refish)) return refish;
    if (refish.startsWith('refs/')) return this.refs.get(refish);
    return this.refs.get(`refs/heads/${refish}`) ?? this.refs.get(`refs/tags/${refish}`);
  }

  /** Create a commit object in the store and return its id. */
  createCommit(tree: string, parents: readonly string[], message: string): string {
    const text = message.endsWith('\n') ? message : `${message}\n`;
    return this.putObject('commit', buildCommitBytes(tree, parents, text));
  }

  /** The commit view of an oid, unwrapping an annotated tag to its target commit. */
  commitOf(oid: string | undefined): { oid: string; view: CommitView } | null {
    if (oid === undefined) return null;
    let current = oid;
    for (let hops = 0; hops < 8; hops += 1) {
      const object = this.objects.get(current);
      if (!object) return null;
      if (object.type === 'commit') return { oid: current, view: parseCommit(object.data) };
      if (object.type === 'tag') {
        current = parseTagTarget(object.data).object;
        continue;
      }
      return null;
    }
    return null;
  }

  /** Every object reachable from `roots` (gitlinks skipped), in no particular order. */
  reachable(roots: Iterable<string>): Set<string> {
    const seen = new Set<string>();
    const stack: string[] = [];
    for (const root of roots) if (isObjectId(root)) stack.push(root);
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      const object = this.objects.get(oid);
      if (!object) continue;
      seen.add(oid);
      if (object.type === 'commit') {
        const commit = parseCommit(object.data);
        stack.push(commit.tree);
        for (const parent of commit.parents) stack.push(parent);
      } else if (object.type === 'tag') {
        stack.push(parseTagTarget(object.data).object);
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) {
          if (entry.mode !== '160000') stack.push(entry.oid);
        }
      }
    }
    return seen;
  }

  /** True when `ancestor` is reachable from `tip` following commit parents (tags unwrapped). */
  isAncestor(ancestor: string, tip: string): boolean {
    if (ancestor === tip) return true;
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (oid === ancestor) return true;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const commit = this.commitOf(oid);
      if (!commit) continue;
      for (const parent of commit.view.parents) stack.push(parent);
    }
    return false;
  }

  /** The merge-base of two commits: the first common ancestor in BFS order from `a`. */
  mergeBase(a: string, b: string): string | null {
    const ancestorsOfB = new Set<string>();
    const stackB = [b];
    while (stackB.length > 0) {
      const oid = stackB.pop() as string;
      if (ancestorsOfB.has(oid)) continue;
      ancestorsOfB.add(oid);
      const commit = this.commitOf(oid);
      if (!commit) continue;
      for (const parent of commit.view.parents) stackB.push(parent);
    }
    const seen = new Set<string>();
    const queueA = [a];
    while (queueA.length > 0) {
      const oid = queueA.shift() as string;
      if (ancestorsOfB.has(oid)) return oid;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const commit = this.commitOf(oid);
      if (!commit) continue;
      for (const parent of commit.view.parents) queueA.push(parent);
    }
    return null;
  }

  /** Commits reachable from `tip`, newest first (BFS from the tip). */
  commitHistory(tip: string | undefined): Array<{ oid: string; view: CommitView }> {
    const out: Array<{ oid: string; view: CommitView }> = [];
    if (tip === undefined) return out;
    const seen = new Set<string>();
    const queue = [tip];
    while (queue.length > 0) {
      const oid = queue.shift() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const commit = this.commitOf(oid);
      if (!commit) continue;
      out.push(commit);
      for (const parent of commit.view.parents) queue.push(parent);
    }
    return out;
  }

  /** Flatten a tree into path -> {oid, mode} for files only. */
  flattenTree(treeOid: string | undefined, prefix = '', out = new Map<string, { oid: string; mode: string }>()): Map<string, { oid: string; mode: string }> {
    if (treeOid === undefined) return out;
    const object = this.objects.get(treeOid);
    if (!object || object.type !== 'tree') return out;
    for (const entry of parseTree(object.data)) {
      const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
      if (entry.mode === '40000') this.flattenTree(entry.oid, path, out);
      else out.set(path, { oid: entry.oid, mode: entry.mode });
    }
    return out;
  }

  /** The tree object id at `path` under `treeOid` ('' is the root), or null. */
  treeAt(treeOid: string | undefined, path: string): string | null {
    if (treeOid === undefined) return null;
    if (path === '') return treeOid;
    let current = treeOid;
    for (const segment of path.split('/')) {
      const object = this.objects.get(current);
      if (!object || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((candidate) => candidate.name === segment && candidate.mode === '40000');
      if (!entry) return null;
      current = entry.oid;
    }
    return current;
  }

  /** The blob entry at `path` under `treeOid`, or null (also reports when the path is a dir). */
  entryAt(treeOid: string | undefined, path: string): { oid: string; mode: string; name: string } | null {
    if (treeOid === undefined) return null;
    const segments = path.split('/').filter((segment) => segment !== '');
    let current = treeOid;
    for (let i = 0; i < segments.length; i += 1) {
      const object = this.objects.get(current);
      if (!object || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((candidate) => candidate.name === segments[i]);
      if (!entry) return null;
      if (i === segments.length - 1) return entry;
      if (entry.mode !== '40000') return null;
      current = entry.oid;
    }
    return null;
  }

  /**
   * A new tree with `path` set to `blobOid` (or removed when null), creating intermediate trees
   * and pruning directories left empty. Returns the new root tree oid.
   */
  updateTree(treeOid: string | undefined, path: string, blobOid: string | null): string {
    const segments = path.split('/').filter((segment) => segment !== '');
    const rebuild = (current: string | undefined, depth: number): string | null => {
      const entries = new Map<string, TreeEntry>();
      if (current !== undefined) {
        const object = this.objects.get(current);
        if (object && object.type === 'tree') {
          for (const entry of parseTree(object.data)) entries.set(entry.name, entry);
        }
      }
      const name = segments[depth] as string;
      if (depth === segments.length - 1) {
        if (blobOid === null) entries.delete(name);
        else entries.set(name, { mode: '100644', name, oid: blobOid });
      } else {
        const child = entries.get(name);
        const next = rebuild(child?.mode === '40000' ? child.oid : undefined, depth + 1);
        if (next === null) entries.delete(name);
        else entries.set(name, { mode: '40000', name, oid: next });
      }
      if (entries.size === 0) return null;
      return this.putObject('tree', buildTree([...entries.values()]));
    };
    return rebuild(treeOid, 0) ?? this.putObject('tree', buildTree([]));
  }
}

// ---------------------------------------------------------------------------
// The forge
// ---------------------------------------------------------------------------

export class Forge {
  readonly viewer = viewerUser();
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 1;
  private globalSeq = 1;
  private commentSeq = 1;

  createRepo(name: string, description: string, privateFlag: boolean): Repo {
    const repo = new Repo(this.nextRepoId, this.viewer.login, name, description, privateFlag);
    this.nextRepoId += 1;
    this.repos.set(`${repo.owner}/${repo.name}`, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    if (owner !== this.viewer.login) return undefined;
    return this.repos.get(`${owner}/${name}`);
  }

  deleteRepo(owner: string, name: string): boolean {
    if (owner !== this.viewer.login) return false;
    return this.repos.delete(`${owner}/${name}`);
  }

  /** Repositories the viewer owns, in creation order. */
  viewerRepos(): Repo[] {
    return [...this.repos.values()].filter((repo) => repo.owner === this.viewer.login);
  }

  nextGlobalId(): number {
    const id = this.globalSeq;
    this.globalSeq += 1;
    return id;
  }

  nextComment(): number {
    const id = this.commentSeq;
    this.commentSeq += 1;
    return id;
  }
}
