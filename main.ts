// main.ts — thin launcher for the native forge. The platform starts every entry with
// `node main.ts`; the forge itself is a static Go binary committed next to this file
// (built from the .go sources in this repo). This file spawns it with the same
// environment, forwards termination signals, and mirrors its exit code.

import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const forgePath = join(dirname(fileURLToPath(import.meta.url)), 'forge');
const child = spawn(forgePath, [], { stdio: 'inherit', env: process.env });

child.on('error', (err) => {
  console.error(`failed to start the forge binary: ${err.message}`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  if (signal) {
    process.kill(process.pid, signal);
  } else {
    process.exit(code ?? 1);
  }
});
process.on('SIGTERM', () => child.kill('SIGTERM'));
process.on('SIGINT', () => child.kill('SIGINT'));
