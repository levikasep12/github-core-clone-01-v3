// Entry lifecycle: bind an ephemeral port, register it with the env, then poll /done until
// the trial is over and exit 0. The forge itself lives in app.ts; this file only owns the
// handshake with the env that runs the suite.
//
//   1. MAKEFUN_ENV_URL names the env origin. Without it there is nothing to register with,
//      so the process exits 1 immediately.
//   2. Listen on port 0 and let the kernel pick a free port. Never 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request
//      came from plus N and dials it from then on. 200 is the normal answer; 409 means
//      another entry already registered first, which is not this process's to fix — it waits
//      like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once
//      ({"done": false} while the suite runs), so this is a poll, not a long-held request.
//   5. Close the server and exit 0. The harness then stops the env, which writes the export.
//
// The trial is replayed and the export must come out byte-identical, so the forge is fully
// deterministic: fixed commit timestamps and REST dates, counter-based request ids, sorted
// pack order, no Date header, no wall clock and no unseeded randomness anywhere.

import { createApp } from './app.ts';

const ENV_ORIGIN = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_ORIGIN === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** One register or done request may take this long before it is abandoned and retried. */
const CALL_TIMEOUT_MS = 30_000;
/** First wait between retries, and the longest wait. */
const RETRY_START_MS = 250;
const RETRY_CAP_MS = 2_000;

const pause = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** Exponential backoff after `attempt` failed requests, capped at RETRY_CAP_MS. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_CAP_MS, RETRY_START_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 400/405 are contract errors and end the
 * program; anything else (a network error, another status) is retried, since the env may
 * still be starting.
 */
async function registerPort(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_ORIGIN}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(CALL_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (error) {
      if (error instanceof Error && error.message.startsWith('register answered HTTP ')) throw error;
    }
    await pause(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done": true}; never return before that. */
async function waitUntilDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_ORIGIN}/done`, { signal: AbortSignal.timeout(CALL_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await pause(retryDelay(attempt));
  }
}

/** Bind, register, wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[entry] listening on port ${address.port}\n`);

  await registerPort(address.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitUntilDone();
  // Let in-flight responses flush before the process exits: close() stops new connections,
  // and awaiting it waits for the existing ones to finish.
  await new Promise<void>((resolve) => server.close(() => resolve()));
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (error) {
  process.stderr.write(`[entry] ${error instanceof Error ? error.message : String(error)}\n`);
  process.exit(1);
}
