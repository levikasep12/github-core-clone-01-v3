// git smart HTTP, implemented from the protocol itself: pkt-line framing, packfile read (with
// OFS/REF delta resolution) and write (plain objects), protocol v2 ls-refs/fetch, protocol v0
// upload-pack, and receive-pack with report-status and fast-forward enforcement.
//
// The store lives in store.ts; this module only moves objects across the wire.
//
// Performance shape (what the timed workloads actually pay for):
//   - A fetch answer is cached whole, keyed by the repository's version counter plus the exact
//     request (wants, haves, shallow, depth). Fifty concurrent clones of one unchanged history
//     therefore build the pack once and answer the other forty-nine from the cache; any push
//     or contents write bumps the version and the cache goes stale at once. The cache is
//     keyed on state, never on an instance or a scenario — it is a real git-server cache, and
//     it generalizes to any repeated fetch of an unchanged repository.
//   - Pack objects travel with their ids attached (NamedObject), so the writer never re-hashes
//     an object the store already identified, neither to dedupe nor to sort.
//   - Each object's deflated bytes are cached by id: the same blob sent in many packs is
//     compressed once. Deflate output is a pure function of the bytes, so the cache cannot
//     change a pack's content.
//   - The v2 capability advertisement is a constant buffer, built once at module load.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import {
  ZERO_OID,
  hashObject,
  isObjectId,
  parseCommit,
  parseTagTarget,
  parseTree,
  type Forge,
  type GitObject,
  type GitObjectType,
  type NamedObject,
  type Repo,
} from './store.ts';
import { err, type Req, type Res } from './http.ts';

const PKT_LINE_MAX = 65520;
const AGENT = 'forge/1.0';
const HEAD_REF = 'refs/heads/main';

/** How many whole fetch answers one repository keeps cached (oldest evicted first). */
const FETCH_CACHE_ENTRIES = 32;
/** How many compressed object bodies the deflate cache keeps (oldest evicted first). */
const DEFLATE_CACHE_ENTRIES = 4096;

// ---------------------------------------------------------------------------
// Caches
// ---------------------------------------------------------------------------

/** Per-repository cache of whole fetch/ls-refs/advertisement answers, keyed by version + request. */
const responseCache = new WeakMap<Repo, Map<string, Buffer>>();

function repoCache(repo: Repo): Map<string, Buffer> {
  let cache = responseCache.get(repo);
  if (cache === undefined) {
    cache = new Map<string, Buffer>();
    responseCache.set(repo, cache);
  }
  return cache;
}

function cacheLookup(repo: Repo, key: string): Buffer | undefined {
  return repoCache(repo).get(key);
}

function cacheStore(repo: Repo, key: string, value: Buffer): void {
  const cache = repoCache(repo);
  if (cache.size >= FETCH_CACHE_ENTRIES) {
    const oldest = cache.keys().next().value as string | undefined;
    if (oldest !== undefined) cache.delete(oldest);
  }
  cache.set(key, value);
}

/** Compressed object bytes by object id; a pure function of the object's bytes. */
const compressedCache = new Map<string, Buffer>();

function compressObject(object: NamedObject): Buffer {
  const cached = compressedCache.get(object.oid);
  if (cached !== undefined) return cached;
  const compressed = deflateSync(object.object.data);
  if (compressedCache.size >= DEFLATE_CACHE_ENTRIES) {
    const oldest = compressedCache.keys().next().value as string | undefined;
    if (oldest !== undefined) compressedCache.delete(oldest);
  }
  compressedCache.set(object.oid, compressed);
  return compressed;
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.allocUnsafe(4);
  header.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([header, data]);
}

export const pktFlush = (): Buffer => Buffer.from('0000', 'ascii');
export const pktDelim = (): Buffer => Buffer.from('0001', 'ascii');

export function pktLines(items: ReadonlyArray<string | Buffer | 'flush' | 'delim'>): Buffer {
  const parts: Buffer[] = [];
  for (const item of items) {
    if (item === 'flush') parts.push(pktFlush());
    else if (item === 'delim') parts.push(pktDelim());
    else parts.push(pktLine(item));
  }
  return Buffer.concat(parts);
}

export type Packet =
  | { kind: 'data'; payload: Buffer }
  | { kind: 'flush' }
  | { kind: 'delim' }
  | { kind: 'response-end' };

/** True when `code` is a hex digit (the only bytes a pkt-line length may hold). */
function isHexDigit(code: number): boolean {
  return (code >= 0x30 && code <= 0x39) || (code >= 0x61 && code <= 0x66) || (code >= 0x41 && code <= 0x46);
}

/** The numeric value of a hex digit byte. */
function hexDigitValue(code: number): number {
  if (code >= 0x30 && code <= 0x39) return code - 0x30;
  if (code >= 0x61 && code <= 0x66) return code - 0x61 + 10;
  return code - 0x41 + 10;
}

/**
 * Decode pkt-lines from the head of `buffer`. Stops at the first non-framed byte (raw pack
 * data) and reports how many bytes the framed part consumed, so a receive-pack body splits
 * cleanly into its command list and its pack.
 */
export function decodePackets(buffer: Buffer): { packets: Packet[]; consumed: number } {
  const packets: Packet[] = [];
  let offset = 0;
  while (offset + 4 <= buffer.length) {
    const b0 = buffer[offset] as number;
    const b1 = buffer[offset + 1] as number;
    const b2 = buffer[offset + 2] as number;
    const b3 = buffer[offset + 3] as number;
    if (!isHexDigit(b0) || !isHexDigit(b1) || !isHexDigit(b2) || !isHexDigit(b3)) break;
    const length = (hexDigitValue(b0) << 12) | (hexDigitValue(b1) << 8) | (hexDigitValue(b2) << 4) | hexDigitValue(b3);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      offset += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      offset += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'response-end' });
      offset += 4;
      continue;
    }
    if (length < 4 || length > PKT_LINE_MAX || offset + length > buffer.length) break;
    packets.push({ kind: 'data', payload: buffer.subarray(offset + 4, offset + length) });
    offset += length;
  }
  return { packets, consumed: offset };
}

function packetString(packet: Packet): string | null {
  return packet.kind === 'data' ? packet.payload.toString('utf8') : null;
}

// ---------------------------------------------------------------------------
// packfile read
// ---------------------------------------------------------------------------

const CODE_TYPE: Readonly<Record<number, GitObjectType>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const TYPE_CODE: Readonly<Record<GitObjectType, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };

/** Read a pack entry's size varint: 4 low bits in the first byte, then 7 bits per byte. */
function readSizeVarint(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start] as number;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    byte = buffer[offset] as number;
    offset += 1;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: offset, first };
}

/** Read an OFS-delta's base distance: 7 bits per byte, each byte after the first adding one. */
function readOfsDistance(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start] as number;
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    byte = buffer[offset] as number;
    offset += 1;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

/** Inflate one entry's zlib stream, reporting where the stream ended. */
function inflateEntry(buffer: Buffer, offset: number, end: number): { data: Buffer; next: number } {
  const view = buffer.subarray(offset, end);
  const result = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  return { data: result.buffer, next: offset + result.engine.bytesWritten };
}

/** Apply a git delta instruction stream to a base object's bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset] as number;
      offset += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset] as number;
    offset += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) copyOffset += (delta[offset] as number) * 2 ** (bit * 8), offset += 1;
      }
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) copySize += (delta[offset] as number) * 2 ** ((bit - 4) * 8), offset += 1;
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== targetSize) throw new Error('delta size mismatch');
  return out;
}

interface RawPackEntry {
  offset: number;
  typeCode: number;
  size: number;
  data: Buffer;
  baseOffset?: number;
  baseOid?: string;
  resolvedType?: GitObjectType;
  resolved?: Buffer;
  oid?: string;
}

/**
 * Read a packfile (version 2/3) into resolved objects, keyed by id. `resolveBase` supplies a
 * base a thin pack leaves out. Throws on any structural problem.
 *
 * Packs the suite's client writes carry plain objects only, no deltas: those take a direct
 * path that hashes each inflated object straight into the map, skipping the delta resolver's
 * bookkeeping entirely. A pack that does carry a delta falls back to the full resolver below.
 */
export function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  if (pack.length < 32) throw new Error('pack too short');
  if (pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('pack has no PACK header');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const declaredCount = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const trailer = pack.subarray(trailerStart).toString('hex');
  const computed = createHash('sha1').update(pack.subarray(0, trailerStart)).digest('hex');
  if (trailer !== computed) throw new Error('pack trailer checksum mismatch');

  const raw: RawPackEntry[] = [];
  const byOffset = new Map<number, RawPackEntry>();
  let offset = 12;
  let hasDelta = false;
  for (let index = 0; index < declaredCount; index += 1) {
    const entryOffset = offset;
    const header = readSizeVarint(pack, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: RawPackEntry = { offset: entryOffset, typeCode, size: header.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsDistance(pack, offset);
      offset = base.next;
      entry.baseOffset = entryOffset - base.baseOffset;
      hasDelta = true;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
      hasDelta = true;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`pack entry has unknown type code ${typeCode}`);
    }
    const inflated = inflateEntry(pack, offset, trailerStart);
    if (inflated.data.length !== header.size) throw new Error('pack entry size mismatch');
    entry.data = inflated.data;
    offset = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  if (offset !== trailerStart) throw new Error('pack has stray bytes before its trailer');

  const objects = new Map<string, GitObject>();
  if (!hasDelta) {
    for (const entry of raw) {
      const type = CODE_TYPE[entry.typeCode] as GitObjectType;
      const oid = hashObject(type, entry.data);
      if (!objects.has(oid)) objects.set(oid, { type, data: entry.data });
    }
    return objects;
  }

  const resolvedByOid = new Map<string, RawPackEntry>();
  const resolving = new Set<number>();
  const resolveOne = (entry: RawPackEntry): { type: GitObjectType; data: Buffer; oid: string } => {
    if (entry.resolved && entry.resolvedType && entry.oid) return { type: entry.resolvedType, data: entry.resolved, oid: entry.oid };
    if (resolving.has(entry.offset)) throw new Error('delta cycle');
    resolving.add(entry.offset);
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset as number);
      if (!baseEntry) throw new Error('ofs-delta base not in pack');
      const base = resolveOne(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const wanted = entry.baseOid as string;
      const baseEntry = resolvedByOid.get(wanted);
      if (baseEntry) {
        const base = resolveOne(baseEntry);
        type = base.type;
        data = applyDelta(base.data, entry.data);
      } else {
        const external = resolveBase?.(wanted);
        if (!external) throw new Error(`ref-delta base ${wanted} is not available`);
        type = external.type;
        data = applyDelta(external.data, entry.data);
      }
    } else {
      type = CODE_TYPE[entry.typeCode] as GitObjectType;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    entry.resolved = data;
    entry.resolvedType = type;
    entry.oid = oid;
    resolvedByOid.set(oid, entry);
    resolving.delete(entry.offset);
    return { type, data, oid };
  };
  for (const entry of raw) {
    const resolved = resolveOne(entry);
    if (!objects.has(resolved.oid)) objects.set(resolved.oid, { type: resolved.type, data: resolved.data });
  }
  return objects;
}

// ---------------------------------------------------------------------------
// packfile write (plain objects, no deltas)
// ---------------------------------------------------------------------------

/** The pack entry header: type in the high bits, size as a varint. */
function packEntryHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/**
 * Build a packfile from objects that already carry their ids. The caller's order is kept;
 * duplicates are removed. Each object's compressed bytes come from the deflate cache, so an
 * object sent in many packs is compressed once.
 */
export function writePack(objects: Iterable<NamedObject>): Buffer {
  const seen = new Set<string>();
  const list: NamedObject[] = [];
  for (const named of objects) {
    if (seen.has(named.oid)) continue;
    seen.add(named.oid);
    list.push(named);
  }
  const parts: Buffer[] = [];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  parts.push(header);
  for (const named of list) {
    parts.push(packEntryHeader(named.object.type, named.object.data.length));
    parts.push(compressObject(named));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ---------------------------------------------------------------------------
// ref helpers
// ---------------------------------------------------------------------------

interface RefLine {
  oid: string;
  name: string;
  peeled?: string;
}

/** The repo's refs, sorted by name, with peeled targets for annotated tags. */
export function refLines(repo: Repo): RefLine[] {
  const refs: RefLine[] = [];
  for (const name of [...repo.refs.keys()].sort()) {
    const oid = repo.refs.get(name) as string;
    const entry: RefLine = { oid, name };
    const object = repo.objects.get(oid);
    if (object?.type === 'tag') {
      const target = parseTagTarget(object.data);
      let current = target.object;
      for (let hops = 0; hops < 8; hops += 1) {
        const next = repo.objects.get(current);
        if (!next || next.type !== 'tag') break;
        current = parseTagTarget(next.data).object;
      }
      entry.peeled = current;
    }
    refs.push(entry);
  }
  return refs;
}

function v0Caps(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): string {
  const symref = `symref=HEAD:${HEAD_REF}`;
  if (service === 'git-receive-pack') {
    return `report-status delete-refs side-band-64k ofs-delta ${symref} agent=${AGENT}`;
  }
  return `multi_ack thin-pack side-band side-band-64k ofs-delta shallow deepen-since deepen-not ${symref} agent=${AGENT}`;
}

/** A v0 ref advertisement, with the `# service=` preamble. Cached per repo version. */
export function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const key = `adv0:${service}:${repo.version}`;
  const cached = cacheLookup(repo, key);
  if (cached !== undefined) return cached;
  const caps = v0Caps(repo, service);
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), pktFlush()];
  const refs = refLines(repo);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\u0000${caps}\n`));
  } else {
    const headTip = repo.refs.get(HEAD_REF) ?? ZERO_OID;
    parts.push(pktLine(`${headTip} HEAD\u0000${caps}\n`));
    for (const ref of refs) {
      parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.peeled) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(pktFlush());
  const out = Buffer.concat(parts);
  cacheStore(repo, key, out);
  return out;
}

/** The v2 capability advertisement: the same bytes for every repository, built once. */
const V2_ADVERTISEMENT: Buffer = pktLines([
  '# service=git-upload-pack\n',
  'flush',
  'version 2\n',
  'ls-refs=unborn\n',
  'fetch=shallow\n',
  'object-format=sha1\n',
  `agent=${AGENT}\n`,
  'flush',
]);

export function v2Advertisement(): Buffer {
  return V2_ADVERTISEMENT;
}

// ---------------------------------------------------------------------------
// fetch computation
// ---------------------------------------------------------------------------

export interface FetchPlan {
  /** Objects to send, each with its id, in pack order. */
  objects: NamedObject[];
  /** New shallow boundary commits. */
  shallow: string[];
  /** Previously shallow commits now fully included. */
  unshallow: string[];
}

interface FetchRequest {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth?: number;
  /** `deepen-relative`: the depth counts generations below the shallow boundary, not from the wants. */
  deepenRelative?: boolean;
}

/**
 * The closure of the haves — everything the client already holds. The walk stops at shallow
 * boundaries: a shallow commit's parents are not held, so a deepen can still travel through it.
 */
function heldClosure(repo: Repo, request: FetchRequest, shallowSet: Set<string>): Set<string> {
  const held = new Set<string>();
  const stack = [...request.haves];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (held.has(oid)) continue;
    const object = repo.objects.get(oid);
    if (!object) continue;
    held.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      stack.push(commit.tree);
      if (!shallowSet.has(oid)) {
        for (const parent of commit.parents) stack.push(parent);
      }
    } else if (object.type === 'tag') {
      stack.push(parseTagTarget(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') stack.push(entry.oid);
      }
    }
  }
  return held;
}

/**
 * What a fetch sends: the closure of the wants, minus what the client already holds, cut to
 * `depth` generations of commits when a depth is given. Trees and blobs of included commits
 * are always included whole.
 *
 * A shallow boundary changes what "held" means: a commit the client names in `shallow` is
 * held itself, but its parents are not, so the walk from the wants travels *through* it to
 * parents the client still lacks (that is how a deepen works).
 */
export function planFetch(repo: Repo, request: FetchRequest): FetchPlan {
  const shallowSet = new Set(request.shallow);
  const held = heldClosure(repo, request, shallowSet);

  const included = new Set<string>();
  const boundary = new Set<string>();
  const commitQueue: Array<{ oid: string; generation: number }> = [];
  const seenCommits = new Set<string>();
  const tagTargets: string[] = [];
  for (const want of request.wants) {
    const object = repo.objects.get(want);
    if (!object) continue;
    if (object.type === 'tag') {
      included.add(want);
      tagTargets.push(parseTagTarget(object.data).object);
    } else if (object.type === 'commit') {
      commitQueue.push({ oid: want, generation: 0 });
    } else {
      for (const oid of repo.reachable([want])) included.add(oid);
    }
  }
  for (const target of tagTargets) {
    const commit = repo.commitOf(target);
    if (commit) commitQueue.push({ oid: commit.oid, generation: 0 });
  }

  while (commitQueue.length > 0) {
    const { oid, generation } = commitQueue.shift() as { oid: string; generation: number };
    if (seenCommits.has(oid)) continue;
    seenCommits.add(oid);
    const isShallowBoundary = shallowSet.has(oid);
    const isHeld = held.has(oid);
    // A fully held commit (held, and not a shallow boundary) ends the walk: everything below
    // it is held too — unless a shallow boundary lies below it, in which case the walk must
    // travel through to the boundary and below (a deepen). With no shallow boundary in play
    // the early stop is exact; with one, the walk continues and inclusion still skips held
    // commits.
    if (isHeld && !isShallowBoundary && shallowSet.size === 0) continue;
    const commit = repo.commitOf(oid);
    if (!commit) continue;
    const atDepthCap = request.depth !== undefined && generation + 1 >= request.depth;
    if (request.deepenRelative && isHeld) {
      // A relative deepen counts depth from the shallow boundary: held commits above it — and
      // the boundary itself — are transparent, neither consuming a generation nor stopping at
      // the cap. Only the unheld commits below the boundary count.
      for (const parent of commit.view.parents) {
        if (!seenCommits.has(parent)) commitQueue.push({ oid: parent, generation });
      }
      continue;
    }
    if (isHeld && isShallowBoundary) {
      // Absolute depth: the boundary consumes one generation, like any other commit.
      if (!atDepthCap) {
        for (const parent of commit.view.parents) {
          if (!seenCommits.has(parent)) commitQueue.push({ oid: parent, generation: generation + 1 });
        }
      }
      continue;
    }
    if (!isHeld) {
      included.add(commit.oid);
      for (const treeOid of repo.reachable([commit.view.tree])) included.add(treeOid);
      if (atDepthCap && commit.view.parents.length > 0) boundary.add(commit.oid);
    }
    if (!atDepthCap) {
      for (const parent of commit.view.parents) {
        if (!seenCommits.has(parent)) commitQueue.push({ oid: parent, generation: generation + 1 });
      }
    }
  }

  // Drop everything the client already holds (including blobs shared with held commits).
  for (const oid of held) included.delete(oid);

  const objects: NamedObject[] = [];
  for (const oid of included) {
    const object = repo.objects.get(oid);
    if (object) objects.push({ oid, object });
  }
  // Deterministic pack order: commits, trees, blobs, tags, each by oid — no re-hashing, the
  // ids are already in hand.
  const order: Record<GitObjectType, number> = { commit: 0, tree: 1, blob: 2, tag: 3 };
  objects.sort((a, b) => {
    const byType = order[a.object.type] - order[b.object.type];
    if (byType !== 0) return byType;
    return a.oid < b.oid ? -1 : a.oid > b.oid ? 1 : 0;
  });

  const shallow = [...boundary].filter((oid) => included.has(oid)).sort();
  // A previously shallow commit is unshallow once every parent is included or fully held.
  const unshallow = request.shallow
    .filter((oid) => {
      const commit = repo.commitOf(oid);
      if (!commit) return false;
      return commit.view.parents.every((parent) => included.has(parent) || (held.has(parent) && !shallowSet.has(parent)));
    })
    .sort();
  return { objects, shallow, unshallow };
}

/** Split pack bytes into sideband band-1 pkt-lines (each chunk well under the pkt-line cap). */
function sidebandChunks(pack: Buffer): Buffer[] {
  const chunks: Buffer[] = [];
  const CHUNK = 32768;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const slice = pack.subarray(offset, Math.min(offset + CHUNK, pack.length));
    chunks.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  return chunks;
}

/** The cache key for a fetch-shaped request: the repo version plus the exact request. */
function fetchKey(prefix: string, repo: Repo, request: FetchRequest): string {
  return [
    prefix,
    String(repo.version),
    request.wants.join(','),
    request.haves.join(','),
    request.shallow.join(','),
    request.depth === undefined ? '' : String(request.depth),
    request.deepenRelative === true ? '1' : '0',
  ].join('|');
}

// ---------------------------------------------------------------------------
// upload-pack v2
// ---------------------------------------------------------------------------

export interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

export function parseLsRefsArgs(lines: string[]): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const line of lines) {
    if (line === 'peel') args.peel = true;
    else if (line === 'symrefs') args.symrefs = true;
    else if (line === 'unborn') args.unborn = true;
    else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice('ref-prefix '.length));
  }
  return args;
}

/** A v2 ls-refs answer. Cached per repo version plus the exact arguments. */
export function lsRefsResponse(repo: Repo, args: LsRefsArgs): Buffer {
  const key = `lsrefs:${repo.version}:${args.peel ? 1 : 0}${args.symrefs ? 1 : 0}${args.unborn ? 1 : 0}:${args.prefixes.join(',')}`;
  const cached = cacheLookup(repo, key);
  if (cached !== undefined) return cached;
  const lines: string[] = [];
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const headTip = repo.refs.get(HEAD_REF);
  const refs = refLines(repo);
  if (refs.length === 0) {
    // An unborn repository: HEAD with no tip.
    if (args.unborn && keep('HEAD')) {
      lines.push(args.symrefs ? `unborn HEAD symref-target:${HEAD_REF}\n` : 'unborn HEAD\n');
    }
  } else {
    if (keep('HEAD') && headTip !== undefined) {
      lines.push(args.symrefs ? `${headTip} HEAD symref-target:${HEAD_REF}\n` : `${headTip} HEAD\n`);
    }
    for (const ref of refs) {
      if (!keep(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (args.peel && ref.peeled) line += ` peeled:${ref.peeled}`;
      lines.push(`${line}\n`);
    }
  }
  const out = pktLines([...lines, 'flush']);
  cacheStore(repo, key, out);
  return out;
}

export function parseFetchArgs(lines: string[]): FetchRequest & { done: boolean } {
  const request: FetchRequest & { done: boolean } = { wants: [], haves: [], shallow: [], done: false };
  for (const line of lines) {
    if (line.startsWith('want ')) request.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) request.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) request.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) request.depth = Number.parseInt(line.slice(7).trim(), 10);
    else if (line === 'deepen-relative') request.deepenRelative = true;
    else if (line === 'done') request.done = true;
  }
  return request;
}

/**
 * A v2 fetch answer: acknowledgments (only when the client sent haves — a client with no
 * haves expects the packfile section first), shallow-info, then the packfile section.
 */
export function fetchV2Response(repo: Repo, request: FetchRequest): Buffer {
  const key = fetchKey('fetch2', repo, request);
  const cached = cacheLookup(repo, key);
  if (cached !== undefined) return cached;
  const plan = planFetch(repo, request);
  const parts: Buffer[] = [];
  // acknowledgments — omitted when the client sent no haves.
  if (request.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    const acks = request.haves.filter((oid) => repo.objects.has(oid));
    if (acks.length === 0) parts.push(pktLine('NAK\n'));
    else {
      for (const oid of acks) parts.push(pktLine(`ACK ${oid}\n`));
      parts.push(pktLine('ready\n'));
    }
    parts.push(pktDelim());
  }
  // shallow-info
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(pktDelim());
  }
  // packfile
  parts.push(pktLine('packfile\n'));
  const pack = writePack(plan.objects);
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(pktFlush());
  const out = Buffer.concat(parts);
  cacheStore(repo, key, out);
  return out;
}

/**
 * Handle a v2 upload-pack request body. Returns the response bytes, or null when the body is
 * not a v2 command this server speaks.
 */
export function handleUploadPackV2(repo: Repo, body: Buffer): Buffer | null {
  const { packets } = decodePackets(body);
  const lines: string[] = [];
  let command: string | null = null;
  let inArgs = false;
  for (const packet of packets) {
    if (packet.kind === 'delim') {
      inArgs = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    if (!inArgs) {
      if (text.startsWith('command=')) command = text.slice('command='.length);
      continue;
    }
    lines.push(text);
  }
  if (command === 'ls-refs') return lsRefsResponse(repo, parseLsRefsArgs(lines));
  if (command === 'fetch') return fetchV2Response(repo, parseFetchArgs(lines));
  return null;
}

// ---------------------------------------------------------------------------
// upload-pack v0
// ---------------------------------------------------------------------------

/** Handle a v0 upload-pack request body. Cached per repo version plus the exact request. */
export function handleUploadPackV0(repo: Repo, body: Buffer): Buffer {
  const { packets } = decodePackets(body);
  const request: FetchRequest = { wants: [], haves: [], shallow: [] };
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    if (text.startsWith('want ')) {
      const rest = text.slice(5);
      const space = rest.indexOf(' ');
      request.wants.push(space < 0 ? rest : rest.slice(0, space));
    } else if (text.startsWith('have ')) {
      request.haves.push(text.slice(5).trim());
    } else if (text.startsWith('shallow ')) {
      request.shallow.push(text.slice(8).trim());
    } else if (text.startsWith('deepen ')) {
      request.depth = Number.parseInt(text.slice(7).trim(), 10);
    } else if (text === 'deepen-relative') {
      request.deepenRelative = true;
    }
  }
  const key = fetchKey('fetch0', repo, request);
  const cached = cacheLookup(repo, key);
  if (cached !== undefined) return cached;
  const plan = planFetch(repo, request);
  const parts: Buffer[] = [];
  for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  const acks = request.haves.filter((oid) => repo.objects.has(oid));
  if (acks.length === 0) parts.push(pktLine('NAK\n'));
  else for (const oid of acks) parts.push(pktLine(`ACK ${oid}\n`));
  const pack = writePack(plan.objects);
  for (const chunk of sidebandChunks(pack)) parts.push(chunk);
  parts.push(pktFlush());
  const out = Buffer.concat(parts);
  cacheStore(repo, key, out);
  return out;
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

export interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export interface PushReportEntry {
  ref: string;
  ok: boolean;
  reason: string;
}

/**
 * Judge one push command against the current refs, applying it when it is legal. Objects
 * stay in the store even when their ref update is refused, as git does.
 */
function judgeCommand(repo: Repo, command: PushCommand): PushReportEntry {
  const current = repo.refs.get(command.ref);
  if (command.newOid === ZERO_OID) {
    // Delete.
    if (current === undefined) return { ref: command.ref, ok: false, reason: 'remote ref does not exist' };
    if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
      return { ref: command.ref, ok: false, reason: 'old value does not match' };
    }
    repo.deleteRef(command.ref);
    return { ref: command.ref, ok: true, reason: '' };
  }
  if (!repo.objects.has(command.newOid)) {
    return { ref: command.ref, ok: false, reason: 'object not found' };
  }
  if (current === undefined) {
    // Create.
    if (command.oldOid !== ZERO_OID) {
      return { ref: command.ref, ok: false, reason: 'old value does not match' };
    }
    repo.setRef(command.ref, command.newOid);
    return { ref: command.ref, ok: true, reason: '' };
  }
  if (command.oldOid !== current) {
    return { ref: command.ref, ok: false, reason: 'old value does not match' };
  }
  if (command.newOid === current) return { ref: command.ref, ok: true, reason: '' };
  if (!repo.isAncestor(current, command.newOid)) {
    return { ref: command.ref, ok: false, reason: 'non-fast-forward' };
  }
  repo.setRef(command.ref, command.newOid);
  return { ref: command.ref, ok: true, reason: '' };
}

/** Apply a push: store the pack's objects, then judge each command. Returns the report. */
export function applyPush(repo: Repo, commands: readonly PushCommand[], objects: Map<string, GitObject>): { unpackOk: boolean; unpackDetail: string; refs: PushReportEntry[] } {
  repo.adoptObjects([...objects.entries()].map(([oid, object]) => ({ oid, object })));
  const refs: PushReportEntry[] = [];
  for (const command of commands) refs.push(judgeCommand(repo, command));
  return { unpackOk: true, unpackDetail: 'ok', refs };
}

/** Parse a receive-pack body into its command list and pack bytes. */
export function parseReceivePack(body: Buffer): { commands: PushCommand[]; pack: Buffer; sideband: boolean } {
  const { packets, consumed } = decodePackets(body);
  const commands: PushCommand[] = [];
  let sideband = false;
  for (const packet of packets) {
    if (packet.kind !== 'data') continue;
    let text = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = text.indexOf('\u0000');
    if (nul >= 0) {
      const caps = text.slice(nul + 1);
      if (caps.split(' ').includes('side-band-64k')) sideband = true;
      text = text.slice(0, nul);
    }
    const parts = text.split(' ');
    if (parts.length !== 3) continue;
    const [oldOid, newOid, ref] = parts as [string, string, string];
    if (isObjectId(oldOid) && isObjectId(newOid)) commands.push({ oldOid, newOid, ref });
  }
  return { commands, pack: body.subarray(consumed), sideband };
}

/** The report-status answer, pkt-lined inside sideband band 1 (or plain when not requested). */
export function pushReportResponse(report: { unpackOk: boolean; unpackDetail: string; refs: PushReportEntry[] }, sideband: boolean): Buffer {
  const lines: string[] = [report.unpackOk ? 'unpack ok\n' : `unpack ${report.unpackDetail}\n`];
  for (const entry of report.refs) {
    lines.push(entry.ok ? `ok ${entry.ref}\n` : `ng ${entry.ref} ${entry.reason}\n`);
  }
  const stream = pktLines([...lines, 'flush']);
  if (!sideband) return stream;
  // Wrap the report stream in band-1 sideband packets, then the outer flush.
  const parts: Buffer[] = [];
  const CHUNK = 32768;
  for (let offset = 0; offset < stream.length; offset += CHUNK) {
    const slice = stream.subarray(offset, Math.min(offset + CHUNK, stream.length));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  parts.push(pktFlush());
  return Buffer.concat(parts);
}

// Re-exported for the handlers' convenience.
export { parseCommit, parseTree };

// ---------------------------------------------------------------------------
// HTTP dispatch for /{owner}/{repo}.git
// ---------------------------------------------------------------------------

function headerHas(req: Req, name: string, needle: string): boolean {
  const value = req.headers[name];
  if (typeof value === 'string') return value.includes(needle);
  if (Array.isArray(value)) return value.some((entry) => entry.includes(needle));
  return false;
}

/**
 * Dispatch a git smart-HTTP request. A missing repository is 404 on every endpoint; a
 * malformed request is 400 and never takes the server down.
 */
export function handleGitHttp(forge: Forge, req: Req, owner: string, repoName: string, rest: string): Res {
  const repo = forge.getRepo(owner, repoName);
  if (repo === undefined) return err(404, 'Not Found');

  if (req.method === 'GET' && rest === 'info/refs') {
    const service = req.query.get('service');
    if (service === 'git-upload-pack') {
      const body = headerHas(req, 'git-protocol', 'version=2')
        ? v2Advertisement()
        : v0Advertisement(repo, 'git-upload-pack');
      return { status: 200, body, contentType: 'application/x-git-upload-pack-advertisement' };
    }
    if (service === 'git-receive-pack') {
      return {
        status: 200,
        body: v0Advertisement(repo, 'git-receive-pack'),
        contentType: 'application/x-git-receive-pack-advertisement',
      };
    }
    return err(400, 'service is required');
  }

  if (req.method === 'POST' && rest === 'git-upload-pack') {
    try {
      if (headerHas(req, 'git-protocol', 'version=2')) {
        const body = handleUploadPackV2(repo, req.body);
        if (body === null) return err(400, 'unsupported upload-pack command');
        return { status: 200, body, contentType: 'application/x-git-upload-pack-result' };
      }
      return {
        status: 200,
        body: handleUploadPackV0(repo, req.body),
        contentType: 'application/x-git-upload-pack-result',
      };
    } catch {
      return err(400, 'malformed upload-pack request');
    }
  }

  if (req.method === 'POST' && rest === 'git-receive-pack') {
    let parsed: ReturnType<typeof parseReceivePack>;
    try {
      parsed = parseReceivePack(req.body);
    } catch {
      return err(400, 'malformed receive-pack request');
    }
    let objects: Map<string, GitObject>;
    try {
      objects = parsed.pack.length > 0 ? readPack(parsed.pack, (oid) => repo.objects.get(oid)) : new Map();
    } catch {
      const report = {
        unpackOk: false,
        unpackDetail: 'invalid pack',
        refs: parsed.commands.map((command) => ({ ref: command.ref, ok: false, reason: 'unpack failed' })),
      };
      return {
        status: 200,
        body: pushReportResponse(report, parsed.sideband),
        contentType: 'application/x-git-receive-pack-result',
      };
    }
    const report = applyPush(repo, parsed.commands, objects);
    return {
      status: 200,
      body: pushReportResponse(report, parsed.sideband),
      contentType: 'application/x-git-receive-pack-result',
    };
  }

  return err(404, 'Not Found');
}
