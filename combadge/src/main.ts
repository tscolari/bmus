// Entry point of the combadge bundle: `combadge.mjs serve|attach [flags]`.
import { main as attach } from "./attach.js";
import { main as serve } from "./serve.js";

const commands: Record<string, (argv: string[]) => Promise<void>> = { serve, attach };

const [cmd, ...args] = process.argv.slice(2);
const run = cmd !== undefined && Object.hasOwn(commands, cmd) ? commands[cmd] : undefined;
if (!run) {
  console.error("usage: combadge serve|attach [flags]");
  process.exit(2);
}
run(args).catch((err: unknown) => {
  console.error(`combadge ${cmd}: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
