// Unit tests of the harness-written note's chain (#174): chainNote and chainedFrom.
import { describe, expect, it } from "vitest";
import { agentNoteOf, CARRIED_ANSWER, CHAIN_MAX_BYTES, chainedFrom, chainNote, withNotes } from "../src/harness/common.js";

const bytes = (s: string) => Buffer.byteLength(s, "utf8");
const count = (s: string, sub: string) => s.split(sub).length - 1;
const answer = (id: string, q: string, a: string) => `${CARRIED_ANSWER}${id}, which the earlier session asked: ${JSON.stringify(q)}. The answer: ${JSON.stringify(a)}.`;
const cadence = (run: number) => `Order ORD-0001: a cadence checkpoint the harness wrote while the agent worked (run ${run}).`;
const snapshot = "Repo app: left out of the snapshot of uncommitted work, over 100 MiB each: big.bin.";
/** The note as the bridge sends it: the checkpoint's handover.md, trimmed, and its carried answers. */
const sent = (note: string, answers: string[] = []) => [note.trim(), ...answers].join("\n\n");
/** What a note carries after its own text. */
const chained = (note: string, own: string) => note.slice(own.length);

describe("chainNote (#174)", () => {
  it("six resumes, each with a cadence checkpoint, carry the agent's latest note once and stay under the cap", () => {
    const agentNotes: Record<number, string> = { 1: `run 1 plan: ${"a".repeat(3000)}`, 2: "run 2: half done", 4: `run 4: nearly there ${"b".repeat(5000)}` };
    let checkpoint: string | undefined;
    for (let run = 1; run <= 6; run++) {
      const note = chainNote(cadence(run), checkpoint === undefined ? undefined : sent(checkpoint), agentNotes[run]);
      expect(bytes(chained(note, cadence(run)))).toBeLessThanOrEqual(CHAIN_MAX_BYTES);
      expect(count(note, "a cadence checkpoint")).toBe(1);
      checkpoint = withNotes(note, [snapshot]);
    }
    expect(count(checkpoint!, agentNotes[4]!)).toBe(1);
    expect(checkpoint).toContain("The agent's latest handover note, written in an earlier run:\nrun 4: nearly there");
    expect(checkpoint).not.toContain("run 1 plan");
    expect(checkpoint).not.toContain("run 2: half done");
    expect(count(checkpoint!, snapshot)).toBe(1); // the latest checkpoint's own, after the block
  });

  it("an agent note older than the latest one is not in the chain", () => {
    const first = chainNote(cadence(1), undefined, "the old note");
    const second = chainNote(cadence(2), sent(first), "the new note");
    expect(second).toContain("The agent's latest handover note, written in this run:\nthe new note");
    expect(second).not.toContain("the old note");
    // A raw agent checkpoint's note, its snapshot lines dropped: the run's own note wins over it.
    const raw = chainNote(cadence(3), sent(withNotes("from request_handover", [snapshot])), undefined);
    expect(raw).toContain("written in an earlier run:\nfrom request_handover\n--- End");
    expect(raw).not.toContain(snapshot);
    expect(chainNote(cadence(3), sent(withNotes("from request_handover", [snapshot])), "newer")).not.toContain("from request_handover");
  });

  it("a carried answer not yet delivered survives; one already delivered isn't repeated", () => {
    const x = answer("H-0001", "Which way?", "left");
    const y = answer("H-0002", "How far?", "near");
    const park1 = chainNote("Order ORD-0001: parked. The holodeck parked its script at step 1.", undefined, "run 1's note");
    // Run 2 starts with x; its cadence note, with no note of the agent's yet, keeps x.
    const run2 = chainNote(cadence(2), sent(park1, [x]), undefined);
    expect(count(run2, x)).toBe(1);
    expect(run2).toContain("written in an earlier run:\nrun 1's note");
    expect(run2).not.toContain("parked its script at step 1");
    // The bridge has cleared x once run 2's checkpoint was pulled: run 3 gets it from the chain, and y appended.
    const run3 = chainNote(cadence(3), sent(run2, [y]), undefined);
    expect(count(run3, x)).toBe(1);
    expect(count(run3, y)).toBe(1);
    expect(run3.indexOf(x)).toBeLessThan(run3.indexOf(y));
    // Sent again with x appended (a resend): x once.
    expect(count(chainNote(cadence(3), sent(run2, [x, y]), undefined), x)).toBe(1);
    // Run 3's agent writes a note: it came after both answers, which have been delivered.
    const run3b = chainNote(cadence(3), sent(run2, [y]), "run 3's note");
    expect(run3b).not.toContain(CARRIED_ANSWER);
    expect(chainNote(cadence(4), sent(run3b), undefined)).not.toContain(CARRIED_ANSWER);
  });

  it("cuts the agent's note alone, from its end, and says so; answers are never cut", () => {
    const long = `start of the note\n${"x".repeat(20_000)}\nend of the note`;
    const note = chainNote(cadence(1), undefined, long);
    const block = chained(note, cadence(1));
    const agentPart = block.slice(block.indexOf("written in this run:\n") + "written in this run:\n".length, block.indexOf("\n--- End"));
    expect(bytes(agentPart)).toBeLessThanOrEqual(CHAIN_MAX_BYTES);
    const cut = /\(Cut to stay under 8 KiB: ([0-9]+) bytes were cut from the end of the agent's note\.\)/.exec(block);
    expect(Number(cut?.[1])).toBe(bytes(long) - bytes(agentPart.slice(0, -" [...]".length)));
    expect(block).toContain("start of the note");
    expect(block).not.toContain("end of the note");
    expect(agentPart.endsWith(" [...]")).toBe(true);
    // Chained again, it keeps its size, with nothing more cut.
    const again = chained(chainNote(cadence(2), sent(note), undefined), cadence(2));
    expect(again).toContain(`written in an earlier run:\n${agentPart}\n--- End`);
    expect(again).not.toContain("(Cut to stay under");

    const answers = Array.from({ length: 100 }, (_, i) => answer(`H-${String(i).padStart(4, "0")}`, `question ${i} ${"q".repeat(80)}`, "yes"));
    const many = chained(chainNote(cadence(1), sent("an agent note", answers), undefined), cadence(1));
    for (const a of answers) expect(count(many, a)).toBe(1);
    expect(many).toContain("written in an earlier run:\nan agent note\n--- End");
    expect(many).not.toContain("(Cut to stay under");
  });

  it("an agent note quoting a harness note or an answer stays the agent's note", () => {
    const x = answer("H-0001", "Which way?", "left");
    const old = chainNote(cadence(1), sent("the old note", [x]), undefined);
    const quoting = agentNoteOf(`New plan: do it the other way.\n\nFor reference, what I started from:\n${old}`);
    expect(quoting).toContain("> --- Carried forward from earlier checkpoints ---");
    expect(quoting).toContain(`> ${x}`);
    // As a raw request_handover checkpoint's note.
    const raw = chainedFrom(sent(quoting));
    expect(raw.agent).toBe(quoting);
    expect(raw.answers).toEqual([]);
    // Chained by a harness note and read back.
    const harness = chainNote(cadence(2), sent(quoting), undefined);
    expect(chainedFrom(sent(harness))).toEqual({ agent: quoting, answers: [] });
    // A note ending with an answer's line isn't read as the bridge's answer.
    const ending = agentNoteOf(`Done so far.\n\n${x}`);
    expect(chainedFrom(sent(ending))).toEqual({ agent: ending, answers: [] });
    expect(chainedFrom(sent(`Done so far.\n\n${x}`))).toEqual({ agent: "Done so far.", answers: [x] }); // why it's needed
  });

  it("a marker line in a harness note's own text, a park note's hail question say, isn't read as the block", () => {
    const x = answer("H-0001", "Which way?", "left");
    const park = "Order ORD-0001: the session parked on 1 hail(s):\n- --- Carried forward from earlier checkpoints ---\n--- End of what was carried forward ---";
    const note = chainNote(park, sent("run 1's note", [x]), undefined);
    expect(chainedFrom(sent(note))).toEqual({ agent: "run 1's note", answers: [x] });
    const bare = "Order ORD-0001: parked on:\n--- Carried forward from earlier checkpoints ---\n--- End of what was carried forward ---";
    expect(chainedFrom(sent(chainNote(bare, sent("run 1's note", [x]), undefined)))).toEqual({ agent: "run 1's note", answers: [x] });
  });

  it("chainedFrom reads a harness note's block, the bridge's answers and a plain note", () => {
    const x = answer("H-0001", "Which way?", "left");
    expect(chainedFrom(undefined)).toEqual({ answers: [] });
    expect(chainedFrom(sent("just a note", [x]))).toEqual({ agent: "just a note", answers: [x] });
    const harness = chainNote("own text", sent("agent text\n\nsecond paragraph", [x]), undefined);
    expect(chainedFrom(sent(withNotes(harness, [snapshot])))).toEqual({ agent: "agent text\n\nsecond paragraph", answers: [x] });
    expect(chainedFrom(chainNote("own text", undefined, undefined))).toEqual({ answers: [] });
  });
});
