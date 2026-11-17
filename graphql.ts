// GitHub GraphQL v4, for the documents gh and github-mcp-server send: a small lexer, a
// recursive-descent parser for the query/mutation subset the suite uses, and an executor over
// the shared store. Introspection answers the __type probes gh feature-detects with.
//
// Supported: named operations with variable definitions, aliases, arguments (variables,
// strings, ints, enums, lists, input objects, null), inline fragments, batched arrays. Not
// supported: fragment definitions and spreads, directives, subscriptions — the suite sends
// none of those.

import {
  commentGlobalId,
  decodeGlobalId,
  issueGlobalId,
  pullGlobalId,
  repoGlobalId,
  type Forge,
  type Issue,
  type PullRequest,
  type Repo,
} from './store.ts';
import { closeIssue as closeIssueOp, createIssue, createPull, mergePull, pullConflicts, pullFiles } from './rest.ts';
import { err, json, type Req, type Res } from './http.ts';

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

interface LexToken {
  kind: 'name' | 'string' | 'int' | 'float' | 'punct' | 'eof';
  value: string;
}

class GqlError extends Error {}

/** Split a GraphQL document into tokens. Throws GqlError on the first bad character. */
function tokenize(input: string): LexToken[] {
  const tokens: LexToken[] = [];
  const emit = (kind: LexToken['kind'], value: string): void => {
    tokens.push({ kind, value });
  };
  let i = 0;
  while (i < input.length) {
    const ch = input[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < input.length && input[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && input.slice(i, i + 3) === '...') {
      emit('punct', '...');
      i += 3;
      continue;
    }
    if ('{}():$![]=@|'.includes(ch)) {
      emit('punct', ch);
      i += 1;
      continue;
    }
    if (ch === '"') {
      if (input.slice(i, i + 3) === '"""') {
        const end = input.indexOf('"""', i + 3);
        if (end < 0) throw new GqlError('unterminated block string');
        emit('string', input.slice(i + 3, end));
        i = end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      while (j < input.length && input[j] !== '"') {
        if (input[j] === '\\' && j + 1 < input.length) {
          const esc = input[j + 1] as string;
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else if (esc === 'b') out += '\b';
          else if (esc === 'f') out += '\f';
          else if (esc === 'u') {
            out += String.fromCharCode(Number.parseInt(input.slice(j + 2, j + 6), 16));
            j += 4;
          } else out += esc;
          j += 2;
          continue;
        }
        out += input[j];
        j += 1;
      }
      if (j >= input.length) throw new GqlError('unterminated string');
      emit('string', out);
      i = j + 1;
      continue;
    }
    if (ch === '-' || (ch >= '0' && ch <= '9')) {
      let j = i;
      if (input[j] === '-') j += 1;
      while (j < input.length && /[0-9]/.test(input[j] as string)) j += 1;
      let floating = false;
      if (input[j] === '.' && /[0-9]/.test(input[j + 1] as string)) {
        floating = true;
        j += 1;
        while (j < input.length && /[0-9]/.test(input[j] as string)) j += 1;
      }
      if (input[j] === 'e' || input[j] === 'E') {
        floating = true;
        j += 1;
        if (input[j] === '+' || input[j] === '-') j += 1;
        while (j < input.length && /[0-9]/.test(input[j] as string)) j += 1;
      }
      emit(floating ? 'float' : 'int', input.slice(i, j));
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < input.length && /[A-Za-z0-9_]/.test(input[j] as string)) j += 1;
      emit('name', input.slice(i, j));
      i = j;
      continue;
    }
    throw new GqlError(`unexpected character ${JSON.stringify(ch)}`);
  }
  emit('eof', '');
  return tokens;
}

// ---------------------------------------------------------------------------
// Parser (AST)
// ---------------------------------------------------------------------------

type ValueExpr =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueExpr[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueExpr }> };

interface FieldSel {
  kind: 'field';
  alias: string;
  name: string;
  args: Array<{ name: string; value: ValueExpr }>;
  selections: Selection[];
}

interface FragmentSel {
  kind: 'fragment';
  typeCondition: string | null;
  selections: Selection[];
}

type Selection = FieldSel | FragmentSel;

interface Operation {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Array<{ name: string; defaultValue: ValueExpr | null }>;
  selections: Selection[];
}

class DocumentParser {
  private tokens: LexToken[];
  private pos = 0;

  constructor(input: string) {
    this.tokens = tokenize(input);
  }

  private peek(): LexToken {
    return this.tokens[this.pos] as LexToken;
  }

  private advance(): LexToken {
    const token = this.tokens[this.pos] as LexToken;
    if (this.pos < this.tokens.length - 1) this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.advance();
    if (token.kind !== 'punct' || token.value !== value) throw new GqlError(`expected "${value}", got ${JSON.stringify(token.value)}`);
  }

  private expectName(value?: string): string {
    const token = this.advance();
    if (token.kind !== 'name') throw new GqlError(`expected a name, got ${JSON.stringify(token.value)}`);
    if (value !== undefined && token.value !== value) throw new GqlError(`expected "${value}", got ${JSON.stringify(token.value)}`);
    return token.value;
  }

  /** Directives are parsed and discarded (the suite sends none that change meaning). */
  private skipDirectives(): void {
    while (this.peek().kind === 'punct' && this.peek().value === '@') {
      this.advance();
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') this.readArguments();
    }
  }

  readDocument(): Operation[] {
    const operations: Operation[] = [];
    while (this.peek().kind !== 'eof') operations.push(this.readOperation());
    return operations;
  }

  private readOperation(): Operation {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const token = this.peek();
    if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      kind = this.advance().value as 'query' | 'mutation';
      if (this.peek().kind === 'name') name = this.advance().value;
    } else if (token.kind === 'name' && token.value === 'subscription') {
      throw new GqlError('subscriptions are not supported');
    } else if (token.kind === 'name' && token.value === 'fragment') {
      throw new GqlError('fragment definitions are not supported');
    }
    const variables = this.readVariableDefinitions();
    this.skipDirectives();
    const selections = this.readSelectionSet();
    return { kind, name, variables, selections };
  }

  private readVariableDefinitions(): Array<{ name: string; defaultValue: ValueExpr | null }> {
    const variables: Array<{ name: string; defaultValue: ValueExpr | null }> = [];
    if (!(this.peek().kind === 'punct' && this.peek().value === '(')) return variables;
    this.advance();
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      this.readType();
      let defaultValue: ValueExpr | null = null;
      if (this.peek().kind === 'punct' && this.peek().value === '=') {
        this.advance();
        defaultValue = this.readValue(true);
      }
      this.skipDirectives();
      variables.push({ name, defaultValue });
    }
    this.expectPunct(')');
    return variables;
  }

  /** A type reference, consumed but not recorded (execution needs no type info). */
  private readType(): void {
    if (this.peek().kind === 'punct' && this.peek().value === '[') {
      this.advance();
      this.readType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    if (this.peek().kind === 'punct' && this.peek().value === '!') this.advance();
  }

  private readSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
      selections.push(this.readSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private readSelection(): Selection {
    if (this.peek().kind === 'punct' && this.peek().value === '...') {
      this.advance();
      let typeCondition: string | null = null;
      if (this.peek().kind === 'name' && this.peek().value === 'on') {
        this.advance();
        typeCondition = this.expectName();
      } else if (this.peek().kind === 'name') {
        throw new GqlError('fragment spreads are not supported');
      }
      this.skipDirectives();
      return { kind: 'fragment', typeCondition, selections: this.readSelectionSet() };
    }
    let name = this.expectName();
    let alias = name;
    if (this.peek().kind === 'punct' && this.peek().value === ':') {
      this.advance();
      alias = name;
      name = this.expectName();
    }
    const args = this.readArguments();
    this.skipDirectives();
    let selections: Selection[] = [];
    if (this.peek().kind === 'punct' && this.peek().value === '{') selections = this.readSelectionSet();
    return { kind: 'field', alias, name, args, selections };
  }

  private readArguments(): Array<{ name: string; value: ValueExpr }> {
    const args: Array<{ name: string; value: ValueExpr }> = [];
    if (!(this.peek().kind === 'punct' && this.peek().value === '(')) return args;
    this.advance();
    while (!(this.peek().kind === 'punct' && this.peek().value === ')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args.push({ name, value: this.readValue(false) });
    }
    this.expectPunct(')');
    return args;
  }

  private readValue(constValue: boolean): ValueExpr {
    const token = this.advance();
    if (token.kind === 'punct' && token.value === '$') {
      if (constValue) throw new GqlError('a variable is not allowed here');
      return { kind: 'variable', name: this.expectName() };
    }
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueExpr[] = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === ']')) values.push(this.readValue(constValue));
      this.expectPunct(']');
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: Array<{ name: string; value: ValueExpr }> = [];
      while (!(this.peek().kind === 'punct' && this.peek().value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.readValue(constValue) });
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new GqlError(`unexpected value ${JSON.stringify(token.value)}`);
  }
}

// ---------------------------------------------------------------------------
// Values and the executor
// ---------------------------------------------------------------------------

/** A typed object value: its GraphQL type name plus its domain payload. */
interface TypedValue {
  t: string;
  v: unknown;
}

const wrap = (t: string, v: unknown): TypedValue => ({ t, v });
const isTyped = (value: unknown): value is TypedValue =>
  typeof value === 'object' && value !== null && 't' in value && 'v' in value && typeof (value as TypedValue).t === 'string';

type Variables = Record<string, unknown>;

function evalValue(node: ValueExpr, variables: Variables): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name];
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
    case 'enum':
      return node.value;
    case 'null':
      return null;
    case 'list':
      return node.values.map((entry) => evalValue(entry, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = evalValue(field.value, variables);
      return out;
    }
  }
}

function evalArgs(field: FieldSel, variables: Variables): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = evalValue(arg.value, variables);
  return out;
}

// ---------------------------------------------------------------------------
// Introspection data
// ---------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'comments', 'labels', 'closed', 'url'],
  PullRequest: ['id', 'number', 'title', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName', 'files', 'body', 'createdAt', 'updatedAt', 'author', 'mergedAt', 'mergeCommit', 'isDraft'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'isPrivate', 'createdAt', 'updatedAt'],
  User: ['id', 'login', 'name', 'email', 'url'],
  Query: ['viewer', 'repository', 'node'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFileConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Ref: ['name', 'target'],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'CODE'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
};

// ---------------------------------------------------------------------------
// Resolvers
// ---------------------------------------------------------------------------

interface ExecCtx {
  forge: Forge;
}

/** Find a repository by its numeric id (the Relay ids only carry the id). */
function findRepoById(forge: Forge, id: number): Repo | undefined {
  for (const repo of forge.repos.values()) if (repo.id === id) return repo;
  return undefined;
}

/** The page cursor for an issue's creation sequence. */
function makeCursor(seq: number): string {
  return Buffer.from(`issue-cursor:${seq}`, 'utf8').toString('base64url');
}

/** The creation sequence a page cursor names, or null when it names none. */
function cursorSeq(cursor: unknown): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^issue-cursor:(\d+)$/.exec(text);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

function issueStateName(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullStateName(pull: PullRequest): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

type FieldResolver = (parent: unknown, args: Record<string, unknown>, ctx: ExecCtx) => unknown;

const FIELD_RESOLVERS: Record<string, Record<string, FieldResolver>> = {
  Query: {
    viewer: (_parent, _args, ctx) => wrap('User', ctx.forge.viewer),
    repository: (_parent, args, ctx) => {
      const repo = ctx.forge.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
      return repo === undefined ? null : wrap('Repository', repo);
    },
    node: (_parent, args, ctx) => {
      const decoded = decodeGlobalId(String(args.id ?? ''));
      if (decoded === null) return null;
      const repo = findRepoById(ctx.forge, decoded.repoId);
      if (repo === undefined) return null;
      switch (decoded.kind) {
        case 'Repository':
          return wrap('Repository', repo);
        case 'Issue': {
          const issue = repo.issues.get(decoded.number);
          return issue === undefined ? null : wrap('Issue', { repo, issue });
        }
        case 'PullRequest': {
          const pull = repo.pulls.get(decoded.number);
          return pull === undefined ? null : wrap('PullRequest', { repo, pull });
        }
        default:
          return null;
      }
    },
    __type: (_parent, args) => {
      const name = String(args.name ?? '');
      if (TYPE_FIELDS[name] !== undefined || TYPE_ENUM_VALUES[name] !== undefined) return wrap('__Type', name);
      return null;
    },
  },
  Mutation: {
    createIssue: (_parent, args, ctx) => {
      const input = (args.input ?? {}) as Record<string, unknown>;
      const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
      if (decoded === null || decoded.kind !== 'Repository') throw new GqlError('createIssue: unknown repositoryId');
      const repo = findRepoById(ctx.forge, decoded.repoId);
      if (repo === undefined) throw new GqlError('createIssue: unknown repositoryId');
      const created = createIssue(ctx.forge, repo, { title: String(input.title ?? ''), body: typeof input.body === 'string' ? input.body : '' });
      if (!created.ok) throw new GqlError(`createIssue: ${created.message}`);
      return wrap('CreateIssuePayload', { repo, issue: created.issue });
    },
    closeIssue: (_parent, args, ctx) => {
      const input = (args.input ?? {}) as Record<string, unknown>;
      const decoded = decodeGlobalId(String(input.issueId ?? ''));
      if (decoded === null || decoded.kind !== 'Issue') throw new GqlError('closeIssue: unknown issueId');
      const repo = findRepoById(ctx.forge, decoded.repoId);
      const issue = repo?.issues.get(decoded.number);
      if (repo === undefined || issue === undefined) throw new GqlError('closeIssue: unknown issueId');
      closeIssueOp(repo, issue);
      return wrap('CloseIssuePayload', { repo, issue });
    },
    createPullRequest: (_parent, args, ctx) => {
      const input = (args.input ?? {}) as Record<string, unknown>;
      const decoded = decodeGlobalId(String(input.repositoryId ?? ''));
      if (decoded === null || decoded.kind !== 'Repository') throw new GqlError('createPullRequest: unknown repositoryId');
      const repo = findRepoById(ctx.forge, decoded.repoId);
      if (repo === undefined) throw new GqlError('createPullRequest: unknown repositoryId');
      const created = createPull(ctx.forge, repo, {
        title: String(input.title ?? ''),
        body: typeof input.body === 'string' ? input.body : '',
        head: String(input.headRefName ?? ''),
        base: String(input.baseRefName ?? repo.defaultBranch),
      });
      if (!created.ok) throw new GqlError(`createPullRequest: ${created.message}`);
      return wrap('CreatePullRequestPayload', { repo, pull: created.pull });
    },
    mergePullRequest: (_parent, args, ctx) => {
      const input = (args.input ?? {}) as Record<string, unknown>;
      const decoded = decodeGlobalId(String(input.pullRequestId ?? ''));
      if (decoded === null || decoded.kind !== 'PullRequest') throw new GqlError('mergePullRequest: unknown pullRequestId');
      const repo = findRepoById(ctx.forge, decoded.repoId);
      const pull = repo?.pulls.get(decoded.number);
      if (repo === undefined || pull === undefined) throw new GqlError('mergePullRequest: unknown pullRequestId');
      const merged = mergePull(repo, pull);
      if (!merged.ok) throw new GqlError(`mergePullRequest: ${merged.message}`);
      return wrap('MergePullRequestPayload', { repo, pull });
    },
    addComment: (_parent, args, ctx) => {
      const input = (args.input ?? {}) as Record<string, unknown>;
      const decoded = decodeGlobalId(String(input.subjectId ?? ''));
      if (decoded === null || decoded.kind !== 'Issue') throw new GqlError('addComment: unknown subjectId');
      const repo = findRepoById(ctx.forge, decoded.repoId);
      const issue = repo?.issues.get(decoded.number);
      if (repo === undefined || issue === undefined) throw new GqlError('addComment: unknown subjectId');
      const comment = { id: ctx.forge.nextComment(), body: String(input.body ?? ''), userLogin: ctx.forge.viewer.login };
      issue.comments.push(comment);
      return wrap('AddCommentPayload', { repo, issue, comment });
    },
  },
  User: {
    login: (parent) => (parent as { login: string }).login,
    id: (parent) => String((parent as { id: number }).id),
    name: (parent) => (parent as { name: string }).name ?? '',
    email: (parent) => (parent as { email: string }).email ?? '',
  },
  Repository: {
    id: (parent) => repoGlobalId(parent as Repo),
    databaseId: (parent) => (parent as Repo).id,
    name: (parent) => (parent as Repo).name,
    nameWithOwner: (parent) => (parent as Repo).fullName,
    owner: (_parent, _args, ctx) => wrap('User', ctx.forge.viewer),
    defaultBranchRef: (parent) => {
      const repo = parent as Repo;
      return repo.defaultTip() === undefined ? null : wrap('Ref', { name: repo.defaultBranch });
    },
    viewerPermission: () => 'ADMIN',
    hasIssuesEnabled: () => true,
    description: (parent) => (parent as Repo).description,
    isPrivate: (parent) => (parent as Repo).privateFlag,
    issue: (parent, args) => {
      const repo = parent as Repo;
      const issue = repo.issues.get(Number(args.number));
      return issue === undefined ? null : wrap('Issue', { repo, issue });
    },
    issues: (parent, args) => {
      const repo = parent as Repo;
      let issues = [...repo.issues.values()];
      const states = args.states;
      if (Array.isArray(states) && states.length > 0) {
        const wanted = new Set(states.map(String));
        issues = issues.filter((issue) => wanted.has(issueStateName(issue)));
      }
      const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
      if (orderBy?.field === 'CREATED_AT' && orderBy.direction === 'DESC') {
        issues.sort((a, b) => b.seq - a.seq || b.number - a.number);
      } else if (orderBy?.field === 'CREATED_AT' && orderBy.direction === 'ASC') {
        issues.sort((a, b) => a.seq - b.seq || a.number - b.number);
      } else {
        issues.sort((a, b) => a.seq - b.seq);
      }
      const after = cursorSeq(args.after);
      if (after !== null) {
        const index = issues.findIndex((issue) => issue.seq === after);
        if (index >= 0) issues = issues.slice(index + 1);
      }
      const first = typeof args.first === 'number' ? args.first : issues.length;
      const page = issues.slice(0, Math.max(0, first));
      return wrap('IssueConnection', { repo, issues: page, hasNextPage: issues.length > page.length });
    },
    pullRequests: (parent, args) => {
      const repo = parent as Repo;
      const pulls = [...repo.pulls.values()].filter((pull) => pull.state === 'open');
      pulls.sort((a, b) => b.seq - a.seq);
      const first = typeof args.first === 'number' ? args.first : pulls.length;
      return wrap('PullRequestConnection', { repo, pulls: pulls.slice(0, Math.max(0, first)) });
    },
    pullRequest: (parent, args) => {
      const repo = parent as Repo;
      const pull = repo.pulls.get(Number(args.number));
      return pull === undefined ? null : wrap('PullRequest', { repo, pull });
    },
  },
  Ref: {
    name: (parent) => (parent as { name: string }).name,
  },
  Issue: {
    id: (parent) => {
      const { repo, issue } = parent as { repo: Repo; issue: Issue };
      return issueGlobalId(repo, issue);
    },
    number: (parent) => (parent as { issue: Issue }).issue.number,
    title: (parent) => (parent as { issue: Issue }).issue.title,
    state: (parent) => issueStateName((parent as { issue: Issue }).issue),
    body: (parent) => (parent as { issue: Issue }).issue.body,
  },
  IssueConnection: {
    nodes: (parent) => {
      const { repo, issues } = parent as { repo: Repo; issues: Issue[] };
      return issues.map((issue) => wrap('Issue', { repo, issue }));
    },
    pageInfo: (parent) => wrap('PageInfo', parent),
  },
  PageInfo: {
    hasNextPage: (parent) => Boolean((parent as { hasNextPage: boolean }).hasNextPage),
    endCursor: (parent) => {
      const { issues } = parent as { issues: Issue[] };
      if (issues.length === 0) return null;
      return makeCursor(issues[issues.length - 1]?.seq ?? 0);
    },
    hasPreviousPage: () => false,
    startCursor: (parent) => {
      const { issues } = parent as { issues: Issue[] };
      if (issues.length === 0) return null;
      return makeCursor(issues[0]?.seq ?? 0);
    },
  },
  PullRequest: {
    id: (parent) => {
      const { repo, pull } = parent as { repo: Repo; pull: PullRequest };
      return pullGlobalId(repo, pull);
    },
    number: (parent) => (parent as { pull: PullRequest }).pull.number,
    title: (parent) => (parent as { pull: PullRequest }).pull.title,
    state: (parent) => pullStateName((parent as { pull: PullRequest }).pull),
    merged: (parent) => (parent as { pull: PullRequest }).pull.merged,
    headRefName: (parent) => (parent as { pull: PullRequest }).pull.headRef,
    baseRefName: (parent) => (parent as { pull: PullRequest }).pull.baseRef,
    mergeable: (parent) => {
      const { repo, pull } = parent as { repo: Repo; pull: PullRequest };
      if (pull.state !== 'open') return 'UNKNOWN';
      return pullConflicts(repo, pull) ? 'CONFLICTING' : 'MERGEABLE';
    },
    files: (parent, args) => {
      const { repo, pull } = parent as { repo: Repo; pull: PullRequest };
      const files = pullFiles(repo, pull).map((file) => ({ path: file.filename }));
      const first = typeof args.first === 'number' ? args.first : files.length;
      return wrap('PullRequestChangedFileConnection', { files: files.slice(0, Math.max(0, first)) });
    },
  },
  PullRequestConnection: {
    nodes: (parent) => {
      const { repo, pulls } = parent as { repo: Repo; pulls: PullRequest[] };
      return pulls.map((pull) => wrap('PullRequest', { repo, pull }));
    },
    pageInfo: () => wrap('PageInfo', { issues: [], hasNextPage: false }),
  },
  PullRequestChangedFileConnection: {
    nodes: (parent) => {
      const { files } = parent as { files: Array<{ path: string }> };
      return files.map((file) => wrap('PullRequestChangedFile', file));
    },
    pageInfo: () => wrap('PageInfo', { issues: [], hasNextPage: false }),
  },
  PullRequestChangedFile: {
    path: (parent) => (parent as { path: string }).path,
  },
  CreateIssuePayload: {
    issue: (parent) => wrap('Issue', parent as { repo: Repo; issue: Issue }),
  },
  CloseIssuePayload: {
    issue: (parent) => wrap('Issue', parent as { repo: Repo; issue: Issue }),
  },
  CreatePullRequestPayload: {
    pullRequest: (parent) => wrap('PullRequest', parent as { repo: Repo; pull: PullRequest }),
  },
  MergePullRequestPayload: {
    pullRequest: (parent) => wrap('PullRequest', parent as { repo: Repo; pull: PullRequest }),
  },
  AddCommentPayload: {
    commentEdge: (parent) => wrap('CommentEdge', parent),
  },
  CommentEdge: {
    node: (parent) => wrap('Comment', parent),
  },
  Comment: {
    id: (parent) => {
      const { repo, comment } = parent as { repo: Repo; comment: { id: number; body: string } };
      return commentGlobalId(repo, comment as { id: number; body: string; userLogin: string });
    },
    body: (parent) => (parent as { comment: { body: string } }).comment.body,
  },
  __Type: {
    name: (parent) => parent as string,
    kind: (parent) => (TYPE_ENUM_VALUES[parent as string] !== undefined ? 'ENUM' : 'OBJECT'),
    fields: (parent) => {
      const fields = TYPE_FIELDS[parent as string];
      if (fields === undefined) return null;
      return fields.map((name) => wrap('__Field', { name }));
    },
    enumValues: (parent) => {
      const values = TYPE_ENUM_VALUES[parent as string];
      if (values === undefined) return null;
      return values.map((name) => wrap('__EnumValue', { name }));
    },
  },
  __Field: {
    name: (parent) => (parent as { name: string }).name,
  },
  __EnumValue: {
    name: (parent) => (parent as { name: string }).name,
  },
};

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

function runSelections(
  typeName: string,
  value: unknown,
  selections: Selection[],
  variables: Variables,
  ctx: ExecCtx,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'fragment') {
      if (selection.typeCondition !== null && selection.typeCondition !== typeName) continue;
      Object.assign(out, runSelections(typeName, value, selection.selections, variables, ctx));
      continue;
    }
    const resolvers = FIELD_RESOLVERS[typeName];
    const resolver = resolvers?.[selection.name];
    if (resolver === undefined) {
      throw new GqlError(`Cannot query field "${selection.name}" on type "${typeName}".`);
    }
    const args = evalArgs(selection, variables);
    const resolved = resolver(value, args, ctx);
    out[selection.alias] = finishValue(resolved, selection.selections, variables, ctx);
  }
  return out;
}

function finishValue(value: unknown, selections: Selection[], variables: Variables, ctx: ExecCtx): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => finishValue(entry, selections, variables, ctx));
  if (isTyped(value)) {
    if (selections.length === 0) return null;
    return runSelections(value.t, value.v, selections, variables, ctx);
  }
  return value;
}

export interface GraphQLOperationInput {
  query?: unknown;
  variables?: unknown;
  operationName?: unknown;
}

export interface GraphQLResult {
  data: unknown;
  errors?: Array<{ message: string }>;
}

export function executeOperation(forge: Forge, input: GraphQLOperationInput): GraphQLResult {
  const ctx: ExecCtx = { forge };
  try {
    if (typeof input.query !== 'string' || input.query.trim() === '') {
      return { data: null, errors: [{ message: 'query is required' }] };
    }
    const variables = (input.variables ?? {}) as Variables;
    const operations = new DocumentParser(input.query).readDocument();
    if (operations.length === 0) return { data: null, errors: [{ message: 'the document holds no operation' }] };
    let operation: Operation | undefined;
    if (typeof input.operationName === 'string' && input.operationName !== '') {
      operation = operations.find((candidate) => candidate.name === input.operationName);
      if (operation === undefined) return { data: null, errors: [{ message: `unknown operation ${JSON.stringify(input.operationName)}` }] };
    } else if (operations.length === 1) {
      operation = operations[0];
    } else {
      return { data: null, errors: [{ message: 'the document holds several operations and no operationName was given' }] };
    }
    const merged: Variables = { ...variables };
    for (const variable of operation.variables) {
      if (merged[variable.name] === undefined && variable.defaultValue !== null) {
        merged[variable.name] = evalValue(variable.defaultValue, {});
      }
    }
    const rootType = operation.kind === 'mutation' ? 'Mutation' : 'Query';
    const data = runSelections(rootType, null, operation.selections, merged, ctx);
    return { data };
  } catch (error) {
    if (error instanceof GqlError) return { data: null, errors: [{ message: error.message }] };
    return { data: null, errors: [{ message: error instanceof Error ? error.message : String(error) }] };
  }
}

/** Handle one POST /api/graphql request: a single operation document, or a batched array. */
export function handleGraphql(forge: Forge, req: Req): Res {
  if (req.jsonParseError) return err(400, 'Malformed JSON body');
  const body = req.json;
  if (Array.isArray(body)) {
    const results = body.map((item) => {
      if (item === null || typeof item !== 'object' || Array.isArray(item)) {
        return { data: null, errors: [{ message: 'each batch item must be an operation document' }] };
      }
      return executeOperation(forge, item as GraphQLOperationInput);
    });
    return json(200, results);
  }
  if (body === null || typeof body !== 'object') return err(400, 'query is required');
  return json(200, executeOperation(forge, body as GraphQLOperationInput));
}
