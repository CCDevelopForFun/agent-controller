import { runSession } from "./adapter.js";
import { emit } from "./wire.js";
import type { CompiledSpec } from "./types.js";

async function readAllStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf8");
}

async function main(): Promise<void> {
  let spec: CompiledSpec;
  try {
    const raw = await readAllStdin();
    spec = JSON.parse(raw) as CompiledSpec;
  } catch (err) {
    process.stderr.write(`agent-runtime: failed to read CompiledSpec from stdin: ${String(err)}\n`);
    process.exit(2);
  }

  let sawError = false;
  const write = (s: string) => process.stdout.write(s);

  try {
    await runSession(spec, (ev) => {
      if (ev.type === "error") sawError = true;
      emit(write, ev);
    });
  } catch (err) {
    sawError = true;
    emit(write, {
      v: 1,
      type: "error",
      ts: new Date().toISOString(),
      sessionId: "unknown",
      data: { message: String(err) },
    });
  }

  process.exit(sawError ? 1 : 0);
}

void main();
