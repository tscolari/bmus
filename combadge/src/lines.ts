import type { Readable } from "node:stream";

/** Calls onLine for each complete "\n"-terminated line read from stream, without the newline. */
export function onLines(stream: Readable, onLine: (line: string) => void): void {
  let buf = "";
  stream.setEncoding("utf8");
  stream.on("data", (chunk: string) => {
    buf += chunk;
    let i: number;
    while ((i = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, i);
      buf = buf.slice(i + 1);
      onLine(line);
    }
  });
}
