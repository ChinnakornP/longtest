/**
 * qa-executor entrypoint.
 *
 * Stage-1 placeholder — the stdio JSON-RPC loop is delivered by T6.
 */
import { UNTRUSTED_END, UNTRUSTED_START, wrapUntrusted } from './untrusted.ts';

export { UNTRUSTED_END, UNTRUSTED_START, wrapUntrusted };

if (process.argv[2] === '--version') {
  process.stdout.write('qa-executor 0.0.0\n');
} else {
  process.stderr.write('qa-executor: JSON-RPC loop not implemented yet (T6)\n');
  process.exitCode = 1;
}
