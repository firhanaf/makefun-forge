// The entry's launcher. The forge itself is a statically linked Linux amd64 Go binary committed at
// bin/forge (built from go/ — `npm run build` rebuilds it). The binary owns the whole lifecycle the env
// expects: it binds an ephemeral port, POSTs {"port": N} to $MAKEFUN_ENV_URL/register, serves until
// GET $MAKEFUN_ENV_URL/done answers {"done": true}, then exits 0. This file only starts it with the same
// environment and stdio, forwards signals, and exits with its code.
//
// /bot is read-only: if the committed file lost its executable bit, a copy in the temp dir is made
// executable and run instead.

import { spawn } from 'node:child_process';
import { accessSync, chmodSync, constants, copyFileSync, mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
// bin/forge is the committed Linux binary the trial runs; bin/forge.exe is a local Windows build for
// grading on a developer machine (git-ignored, never shipped).
const shipped = join(here, 'bin', process.platform === 'win32' ? 'forge.exe' : 'forge');

if ((process.env.MAKEFUN_ENV_URL ?? '') === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

function executable(path: string): string {
  try {
    accessSync(path, constants.X_OK);
    return path;
  } catch {
    const copy = join(mkdtempSync(join(tmpdir(), 'forge-')), 'forge');
    copyFileSync(path, copy);
    chmodSync(copy, 0o755);
    return copy;
  }
}

const child = spawn(executable(shipped), [], { stdio: 'inherit', env: process.env });

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => child.kill(signal));
}

child.on('error', (err) => {
  process.stderr.write(`[entry] could not start the forge: ${err.message}\n`);
  process.exit(1);
});

child.on('exit', (code, signal) => {
  process.exit(code ?? (signal === null ? 1 : 1));
});
