import * as path from "node:path";
import { CATEGORY_CLARIFYING_QUESTION, type AnswerBody } from "../protocol.js";
import { orderDir, Stopped, writePlaceholderCheckpoint } from "./common.js";
import type { Emit, Harness, HarnessOptions, OnInbound, Order } from "./types.js";

/**
 * A stand-in harness with no agent, so `serve` can be exercised end to end
 * before the real harnesses exist (sdk: #11, holodeck: #12). For each order it
 * emits a progress, hails the captain with the order title, and reports the
 * answer's decision as the summary. A `belay` rejects the run. A `stand_down`
 * or `recall` writes a placeholder checkpoint into `<work-dir>/<order>/1/`
 * at once (see writePlaceholderCheckpoint), emits it as checkpoint_ready with
 * `stopping: true` and rejects the run with Stopped; a belay before that
 * emit wins. It runs no model, so it
 * ignores the order's `model`.
 */
export function echoHarness(opts: HarnessOptions): Harness {
  const workDir = path.resolve(opts.workDir);
  return {
    name: "echo",
    run(order: Order, emit: Emit, onInbound: OnInbound) {
      return new Promise((resolve, reject) => {
        const ref = `echo-${order.id}`;
        let ended = false; // the run's outcome is decided
        let stopping = false; // a stop is writing its checkpoint; a belay may still win
        onInbound((m) => {
          if (ended) return;
          if (m.type === "answer" && m.body.ref === ref && !stopping) {
            ended = true;
            resolve(report(order, m.body));
          } else if (m.type === "belay") {
            ended = true;
            reject(new Error(`belayed: ${m.body.reason ?? "no reason given"}`));
          } else if ((m.type === "stand_down" || m.type === "recall") && !stopping) {
            stopping = true;
            const stop = new Stopped(m.type, m.body.reason);
            writePlaceholderCheckpoint(path.join(orderDir(workDir, order), "1"), order, { echo: true, order: order.id }, `Order ${order.id}: ${stop.message}. Echo did no work.`).then(
              (body) => {
                if (ended) return;
                ended = true;
                emit({ type: "checkpoint_ready", body: { ...body, stopping: true } });
                reject(stop);
              },
              (err: unknown) => {
                if (ended) return;
                ended = true;
                reject(err as Error);
              },
            );
          }
        });
        emit({ type: "progress", body: { pct: 0, note: `received ${order.body.title}` } });
        emit({
          type: "hail",
          body: { ref, category: CATEGORY_CLARIFYING_QUESTION, question: order.body.title },
        });
      });
    },
  };
}

function report(_order: Order, answer: AnswerBody) {
  return {
    summary: answer.decision,
    usage: { input_tokens: 0, output_tokens: 0 },
  };
}
