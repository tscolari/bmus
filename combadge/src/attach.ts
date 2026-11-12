import * as net from "node:net";
import { parseArgs } from "node:util";

/**
 * `combadge attach --socket <path> --since <seq>`: the short-lived relay the
 * bridge's pump launches once per connection (plan §5, §6). Dials serve's
 * socket, sends the `{"since":N}` handshake, then copies bytes both ways
 * between the socket and its own stdin/stdout. Exits when the socket closes
 * or stdin hits EOF; reconnecting is the pump's job.
 */
export async function main(argv: string[]): Promise<void> {
  const { values } = parseArgs({
    args: argv,
    options: {
      socket: { type: "string" },
      since: { type: "string", default: "0" },
    },
    strict: true,
  });
  if (!values.socket) throw new Error("--socket is required");
  const since = Number(values.since);
  if (!Number.isSafeInteger(since) || since < 0) throw new Error("--since must be a non-negative integer");

  const sock = net.connect(values.socket);
  await new Promise<void>((resolve, reject) => {
    sock.once("connect", resolve);
    sock.once("error", reject);
  });
  sock.on("error", (err) => {
    console.error(`combadge attach: ${err.message}`);
    process.exitCode = 1;
  });

  // The pump closing its end of our stdout (EPIPE) ends the session like EOF does.
  process.stdout.on("error", () => {
    sock.destroy();
    process.stdin.destroy();
  });

  sock.write(JSON.stringify({ since }) + "\n");
  sock.pipe(process.stdout);
  process.stdin.pipe(sock); // stdin EOF half-closes the socket; serve then closes its side.
  sock.on("close", () => process.stdin.destroy());
}
