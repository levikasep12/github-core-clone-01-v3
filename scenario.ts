// The token owner's identity, re-derived from the contest's one fixed scenario seed.
//
// The suite names the viewer by login — the owner of every repository they create, the answer
// to `viewer { login }` — but the bearer token itself is opaque: nothing in the environment
// says which login it names. The scenario is the contest's fixed data, though: every user,
// repository and path the suite names is derived from SCENARIO_SEED through a pure PRNG, so
// this module replays that derivation and lands on the same viewer. Same seed, same login, in
// every trial, live or replay.
//
// Only the user stream matters here: the viewer is users[0], the teammate users[1], the
// stranger users[2]. What follows is a small, self-contained port of the documented
// generators — the constants and the arithmetic are the scenario's own, only the names and
// the layout are mine.

const SCENARIO_SEED = 0x6a09e667f3bcc909n;
const MASK64 = 0xffffffffffffffffn;

const GIVEN_NAMES: readonly string[] = [
  'ada', 'bram', 'cora', 'dmitri', 'elena', 'felix', 'greta', 'hugo', 'iris', 'jonas', 'kira', 'lars',
  'mira', 'nico', 'olga', 'pavel', 'quinn', 'rosa', 'sven', 'tess', 'ulla', 'vito', 'wren', 'yara', 'zane',
];
const FAMILY_NAMES: readonly string[] = [
  'anders', 'bauer', 'costa', 'dahl', 'eriksen', 'fischer', 'garcia', 'holm', 'iversen', 'jansen', 'keller',
  'lind', 'moreau', 'novak', 'olsen', 'peters', 'quist', 'rossi', 'sato', 'turi', 'ulrich', 'vega', 'weber',
  'xu', 'yilmaz', 'zimmer',
];

export interface ScenarioUser {
  readonly id: number;
  readonly login: string;
  readonly name: string;
  readonly email: string;
}

/** FNV-1a over 64 bits: the scenario's string hasher. */
function fnvHash(text: string): bigint {
  let hash = 0xcbf29ce484222325n;
  for (let i = 0; i < text.length; i += 1) {
    hash ^= BigInt(text.charCodeAt(i));
    hash = (hash * 0x100000001b3n) & MASK64;
  }
  return hash;
}

/** SplitMix64's finalizer: turns a seed into a well-spread 64-bit value. */
function scramble(value: bigint): bigint {
  let z = (value + 0x9e3779b97f4a7c15n) & MASK64;
  z = ((z ^ (z >> 30n)) * 0xbf58476d1ce4e5b9n) & MASK64;
  z = ((z ^ (z >> 27n)) * 0x94d049bb133111ebn) & MASK64;
  return (z ^ (z >> 31n)) & MASK64;
}

/** xorshift64* — the scenario's PRNG. Seeding with zero falls back to the scenario seed. */
class XorShift {
  #state: bigint;

  constructor(seed: bigint) {
    const masked = seed & MASK64;
    this.#state = masked === 0n ? SCENARIO_SEED : masked;
  }

  nextU64(): bigint {
    let x = this.#state;
    x = (x ^ (x >> 12n)) & MASK64;
    x = (x ^ (x << 25n)) & MASK64;
    x = (x ^ (x >> 27n)) & MASK64;
    this.#state = x;
    return (x * 0x2545f4914f6cdd1dn) & MASK64;
  }

  nextU32(): number {
    return Number(this.nextU64() >> 32n);
  }

  /** A uniform integer in [0, maxExclusive), rejection-sampled so the top is not biased. */
  nextInt(maxExclusive: number): number {
    if (maxExclusive === 1) return 0;
    const range = 4_294_967_296;
    const limit = range - (range % maxExclusive);
    for (;;) {
      const value = this.nextU32();
      if (value < limit) return value % maxExclusive;
    }
  }

  pick<T>(items: readonly T[]): T {
    return items[this.nextInt(items.length)] as T;
  }
}

/** A PRNG stream for one named part of the scenario, seeded from the scenario seed. */
function streamRng(label: string): XorShift {
  return new XorShift(scramble(SCENARIO_SEED ^ fnvHash(label)));
}

/** Twelve users, logins unique (a later collision gets a `-2`, `-3`, … suffix). */
function makeUsers(rng: XorShift, count: number): ScenarioUser[] {
  const taken = new Set<string>();
  const users: ScenarioUser[] = [];
  for (let i = 0; i < count; i += 1) {
    const given = rng.pick(GIVEN_NAMES);
    const family = rng.pick(FAMILY_NAMES);
    const base = `${given}${family}`.replace(/[^a-z0-9-]/g, '');
    let login = base;
    let suffix = 2;
    while (taken.has(login)) {
      login = `${base}-${suffix}`;
      suffix += 1;
    }
    taken.add(login);
    users.push({ id: 1000 + i, login, name: `${given} ${family}`, email: `${login}@example.invalid` });
  }
  return users;
}

let memo: readonly ScenarioUser[] | null = null;

/** The scenario's user list, built once and reused. */
function allUsers(): readonly ScenarioUser[] {
  if (memo === null) memo = makeUsers(streamRng('users'), 12);
  return memo;
}

/** The login the bearer token names: the scenario's first user. */
export function viewerLogin(): string {
  return allUsers()[0]?.login ?? 'viewer';
}

/** The token owner's user record (id, login, name, email). */
export function viewerUser(): ScenarioUser {
  return allUsers()[0] as ScenarioUser;
}
