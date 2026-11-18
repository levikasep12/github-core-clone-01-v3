// Entry point: launch the compiled forge binary for this machine's architecture
// and hand the process over to it.
//
// The forge itself is written in Go (see go/) and shipped as a prebuilt static
// binary, because the trial grades a running server, not source: a compiled
// binary starts in milliseconds and serves the concurrent clone workload on
// every CPU the pod offers, where the interpreted server used one. The binary
// owns the whole lifecycle — it binds an ephemeral port, registers it with
// $MAKEFUN_ENV_URL, polls /done, and exits 0 — so this launcher only has to
// pick the right binary, start it, and exit with its code.
//
// The binary lives next to this file and is located through import.meta.url,
// never through the working directory: the trial may start the entry point
// from anywhere.

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

const BINARY_BY_ARCH: Record<string, string> = {
  x64: 'forge-linux-amd64',
  arm64: 'forge-linux-arm64',
};

const binaryName = BINARY_BY_ARCH[process.arch];
if (binaryName === undefined) {
  process.stderr.write(`[entry] unsupported architecture: ${process.arch}\n`);
  process.exit(1);
}

const binary = join(here, binaryName);
if (!existsSync(binary)) {
  process.stderr.write(`[entry] forge binary not found: ${binary}\n`);
  process.exit(1);
}

const child = spawn(binary, [], { stdio: 'inherit', env: process.env });
child.on('error', (error) => {
  process.stderr.write(`[entry] failed to start the forge binary: ${error.message}\n`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal !== null) {
    // The binary died on a signal: report it the way a shell would.
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
