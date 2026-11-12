// The console end to end (#77): a holodeck bridge, one crew member, and the
// console in Chromium. An order goes away, hails, is answered and comes back
// done, all through the feed, with no reload. Hail notifications (#76): one
// per hail opened live, none for the hails the page loaded with. The Phase 2
// controls (#78): standing orders, withdraw, route, belay, recall and
// re-energize, each through the page and seen through the feed. Countermand
// (#153): a captain timeout's default option, overridden from the page.
// Order split (#155): Command's split, approved from the page, waits on
// its open children. Cascade belay (#146): belaying the split order from
// its record row withdraws its children. The order form's repo dropdown
// (#211): the bridge's registered repos, and none. Push (#4): a done
// order's work pushed to the bare remote from its record row, a diverged
// target forced past a second confirm. The mission report (#104): a done
// order, its crew, beam, report and diff in a new tab, the diff copied to
// the clipboard.
import { expect, test, type Page, type TestInfo } from "@playwright/test";
import { execFile, spawn, type ChildProcess } from "node:child_process";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { promisify } from "node:util";

declare global {
  interface Window {
    __bmusLoaded?: boolean;
    __csp?: string[];
    __notes?: FakeNote[];
  }
}

/** What the init script's stand-in for Notification records. */
interface FakeNote {
  title: string;
  body: string;
  tag: string;
  closed: boolean;
  onclick: (() => void) | null;
}

const execFileP = promisify(execFile);
const script = path.join(import.meta.dirname, "holodeck.json");
/** A hail with the sdk harness's {tool, input} context, which Regulations can read (#153). */
const countermandScript = path.join(import.meta.dirname, "countermand.json");
/** Command splitting its order into two science orders (#155). */
const splitScript = path.join(import.meta.dirname, "split.json");
/** A mission that commits on bmus/<order> and reports, so its work comes back (#4). */
const workScript = path.join(import.meta.dirname, "work.json");
const missionTimeout = 60_000;

function env(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is not set (global setup sets it)`);
  return v;
}

function until(ms: number) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// alive reports whether pid exists.
function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function git(cwd: string, ...args: string[]) {
  await execFileP("git", ["-c", "user.name=bmus e2e", "-c", "user.email=e2e@bmus.invalid", ...args], { cwd });
}

/** Runs git in cwd and returns its trimmed stdout. */
async function gitOut(cwd: string, ...args: string[]): Promise<string> {
  const { stdout } = await execFileP("git", args, { cwd });
  return stdout.trim();
}

/** A `bmus bridge run --transporter holodeck` in its own short temp dir. */
class Bridge {
  readonly dir: string;
  readonly home: string;
  readonly root: string;
  readonly log: string;
  private proc: ChildProcess | undefined;
  private exited: Promise<void> | undefined;
  private done = false;
  private orders = 0;

  constructor() {
    // Beam socket paths live under the holodeck root and must stay under
    // the ~100 byte Unix socket limit.
    const base = os.tmpdir().length > 20 ? "/tmp" : os.tmpdir();
    this.dir = fs.mkdtempSync(path.join(base, "bme"));
    this.home = path.join(this.dir, "h");
    this.root = path.join(this.dir, "hd");
    this.log = path.join(this.dir, "bridge.log");
  }

  async bmus(...args: string[]): Promise<string> {
    const { stdout } = await execFileP(env("BMUS_E2E_BIN"), ["--home", this.home, ...args], {
      env: { ...process.env, BMUS_HOME: "" },
      timeout: 30_000,
    });
    return stdout;
  }

  /**
   * Starts the bridge with a bare repo "app" and returns the console URL.
   * opts.regs is its regs.yaml, if given; opts.script its holodeck script,
   * holodeck.json by default; opts.repos the names it registers, each a
   * bare clone of the same work repo, ["app"] by default.
   */
  async start(opts: { regs?: string; script?: string; repos?: string[] } = {}): Promise<string> {
    const { regs } = opts;
    if (regs !== undefined) {
      fs.mkdirSync(this.home, { recursive: true, mode: 0o700 });
      fs.writeFileSync(path.join(this.home, "regs.yaml"), regs, { mode: 0o600 });
    }
    const work = path.join(this.dir, "work");
    fs.mkdirSync(work);
    await git(work, "init", "-q", "-b", "main");
    fs.writeFileSync(path.join(work, "README.md"), "app\n");
    await git(work, "add", "README.md");
    await git(work, "commit", "-q", "-m", "init");
    const repoArgs: string[] = [];
    for (const name of opts.repos ?? ["app"]) {
      const repo = path.join(this.dir, `${name}.git`);
      await git(this.dir, "clone", "-q", "--bare", work, repo);
      repoArgs.push("--repo", `${name}=${repo}`);
    }

    const out = fs.openSync(this.log, "a");
    // prettier-ignore
    this.proc = spawn(env("BMUS_E2E_BIN"), [
      "--home", this.home, "bridge", "run", "--transporter", "holodeck",
      "--combadge", env("BMUS_COMBADGE"), "--holodeck-root", this.root, "--holodeck-script", opts.script ?? script,
      ...repoArgs, "--log-level", "debug",
    ], { stdio: ["ignore", out, out], env: { ...process.env, BMUS_HOME: "" } });
    fs.closeSync(out);
    const proc = this.proc;
    this.exited = new Promise((resolve) => proc.once("exit", () => resolve()));
    void this.exited.then(() => (this.done = true));

    // bmus console fails until the bridge serves RPC and the console.
    const deadline = Date.now() + 30_000;
    for (;;) {
      if (this.done) throw new Error("bridge exited during start");
      try {
        const url = (await this.bmus("console", "--no-open")).trim();
        if (url.startsWith("http://127.0.0.1:")) return url;
      } catch (err) {
        if (Date.now() > deadline) throw err;
      }
      await until(200);
    }
  }

  /** Creates an order directed to crewId on repo app and returns its id. */
  async order(crewId: string, title: string): Promise<string> {
    return this.createOrder({ title, assignee: crewId });
  }

  /** Creates an order with fields (title, assignee, division) on repo app and returns its id. */
  async createOrder(fields: Record<string, string>): Promise<string> {
    const file = path.join(this.dir, `order-${this.orders++}.yaml`);
    const lines = Object.entries(fields).map(([k, v]) => `${k}: ${v}\n`);
    fs.writeFileSync(file, `${lines.join("")}repos:\n  - name: app\n    base_ref: main\n`);
    return (await this.bmus("order", "create", file)).trim();
  }

  /** SIGKILLs every beam's serve process group: the beams are lost. */
  killBeams() {
    for (const pid of this.servePIDs()) {
      try {
        process.kill(-pid, "SIGKILL");
      } catch {
        // gone
      }
    }
  }

  /** Waits for the open hail of orderId and returns its id. */
  async openHail(orderId: string): Promise<string> {
    const deadline = Date.now() + missionTimeout;
    for (;;) {
      const { hails } = JSON.parse(await this.bmus("hails", "list", "--json")) as { hails: { id: string; order_id: string }[] };
      const h = hails.find((h) => h.order_id === orderId);
      if (h) return h.id;
      if (Date.now() > deadline) throw new Error(`no open hail for ${orderId}`);
      await until(250);
    }
  }

  /** SIGTERM, a wait, then SIGKILL; then the beams' serve processes, which outlive the bridge. */
  async stop() {
    if (this.proc && this.exited && !this.done) {
      this.proc.kill("SIGTERM");
      const stopped = await Promise.race([this.exited.then(() => true), until(20_000).then(() => false)]);
      if (!stopped) {
        this.proc.kill("SIGKILL");
        await this.exited;
      }
    }
    for (const pid of this.servePIDs()) {
      try {
        process.kill(-pid, "SIGTERM");
      } catch {
        continue;
      }
      const deadline = Date.now() + 5_000;
      while (alive(pid) && Date.now() < deadline) await until(100);
      try {
        process.kill(-pid, "SIGKILL");
      } catch {
        // gone
      }
    }
  }

  private servePIDs(): number[] {
    let beams: string[];
    try {
      beams = fs.readdirSync(this.root);
    } catch {
      return [];
    }
    return beams
      .map((b) => {
        try {
          return Number.parseInt(fs.readFileSync(path.join(this.root, b, "serve.pid"), "utf8").trim(), 10);
        } catch {
          return NaN;
        }
      })
      .filter((pid) => pid > 0);
  }

  async attachLogs(info: TestInfo) {
    const read = (f: string) => {
      try {
        return fs.readFileSync(f);
      } catch {
        return undefined;
      }
    };
    const log = read(this.log);
    if (log) await info.attach("bridge.log", { body: log, contentType: "text/plain" });
    let beams: string[] = [];
    try {
      beams = fs.readdirSync(this.root);
    } catch {
      // no beams
    }
    for (const b of beams) {
      const serve = read(path.join(this.root, b, "serve.log"));
      if (serve) await info.attach(`${b}-serve.log`, { body: serve, contentType: "text/plain" });
    }
  }

  remove() {
    fs.rmSync(this.dir, { recursive: true, force: true });
  }
}

let bridge: Bridge | undefined;

test.afterEach(async ({}, info) => {
  // Teardown shares the test's timeout; give it its own room.
  test.setTimeout(info.timeout + 120_000);
  if (!bridge) return;
  try {
    await bridge.stop();
    if (info.status !== info.expectedStatus) await bridge.attachLogs(info);
  } finally {
    bridge.remove();
    bridge = undefined;
  }
});

test("an order goes away, hails, is answered and comes back done", async ({ page }) => {
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error") errors.push(m.text());
  });
  page.on("pageerror", (e) => errors.push(e.message));
  await page.addInitScript(() => {
    window.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => window.__csp?.push(`${e.violatedDirective} ${e.blockedURI}`));
  });

  bridge = new Bridge();
  const url = await bridge.start();
  await page.goto(url);

  const condition = page.getByRole("status").filter({ hasText: "CONDITION" });
  await expect(condition).toContainText("GREEN");
  await page.evaluate(() => {
    window.__bmusLoaded = true;
  });

  // The crew member arrives through the feed.
  await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--rank", "Lieutenant", "--division", "science");
  const sorell = page.getByRole("article", { name: "Lt. Sorell" });
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });

  // Issue an order directed to them, on repo app.
  const title = "Add retries to the notifications webhook sender";
  await sorell.getByRole("button", { name: "Direct the next order to Lt. Sorell" }).click();
  await expect(page.getByLabel("Send to")).toHaveValue("sorell");
  await page.getByLabel("Order", { exact: true }).fill(title);
  await page.getByRole("button", { name: "Yellow alert" }).click();
  // The bridge's one repo is preselected (#211).
  await expect(page.getByLabel("Repo", { exact: true })).toHaveValue("app");
  await page.getByRole("button", { name: "Engage" }).click();
  const issued = page.getByRole("status").filter({ hasText: "issued" });
  await expect(issued).toHaveText(/^ORD-\d+ issued\.$/);
  const orderId = (await issued.innerText()).split(" ")[0] ?? "";

  // It goes away and reports progress.
  const away = page.getByRole("region", { name: /^Away missions/ });
  await expect(away.getByText(title)).toBeVisible({ timeout: missionTimeout });
  await expect(away.getByText("surveying the codebase")).toBeVisible({ timeout: missionTimeout });
  await expect(away.getByRole("progressbar", { name: `${orderId} progress` })).toHaveAttribute("aria-valuenow", "10");

  // It hails. The question is the crew member's text, never HTML.
  const hails = page.getByRole("region", { name: "Hailing frequencies open" });
  await expect(hails).toBeVisible({ timeout: missionTimeout });
  await expect(hails.getByText('"Run <b>x</b> make deploy?"', { exact: true })).toBeVisible();
  await expect(hails.locator("b")).toHaveCount(0);
  await expect(condition).toContainText("YELLOW");
  // The order is hailing while the hail is open (#134), and so is the crew card.
  const row = away.locator(".row").filter({ hasText: orderId });
  await expect(row.locator(".row-state")).toHaveText("Hailing");
  await expect(sorell).toContainText("Hailing");

  // Answer it; it closes, the order is back away and the mission finishes.
  await hails.getByRole("button", { name: "allow", exact: true }).click();
  await expect(hails).toBeHidden({ timeout: missionTimeout });
  await expect(row.locator(".row-state")).toHaveText("Away");

  const record = page.getByRole("region", { name: "Mission record" });
  await expect(record.getByText(orderId, { exact: true })).toBeVisible({ timeout: missionTimeout });
  await expect(record).toContainText(title);
  await expect(record).toContainText("retries added to the webhook sender");
  await expect(record).toContainText("Complete");
  await expect(away.getByText("Nobody is on an away mission.")).toBeVisible({ timeout: missionTimeout });
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });
  await expect(condition).toContainText("GREEN");

  // All of it live, on the page first loaded.
  expect(await page.evaluate(() => window.__bmusLoaded)).toBe(true);
  expect(await page.evaluate(() => window.__csp)).toEqual([]);
  expect(errors).toEqual([]);
});

test("the order form offers the bridge's registered repos (#211)", async ({ page }) => {
  bridge = new Bridge();
  await page.goto(await bridge.start({ repos: ["app", "lib"] }));
  const repo = page.getByLabel("Repo", { exact: true });
  await expect(repo.locator("option")).toHaveText(["app", "lib"]);
  await expect(repo).toHaveValue("app");
  await expect(page.getByLabel("Base ref")).toHaveValue("main");

  // A pool order on lib: standing orders are off, so it stays there.
  await page.getByLabel("Order", { exact: true }).fill("Bump the lib");
  await page.getByLabel("Division").fill("science");
  await repo.selectOption("lib");
  await page.getByRole("button", { name: "Engage" }).click();
  const issued = page.getByRole("status").filter({ hasText: "issued" });
  await expect(issued).toHaveText(/^ORD-\d+ issued\.$/);
  const orderId = (await issued.innerText()).split(" ")[0] ?? "";
  const { orders } = JSON.parse(await bridge.bmus("order", "list", "--json")) as { orders: { id: string; repos: { name: string; base_ref: string }[] }[] };
  expect(orders.find((o) => o.id === orderId)?.repos).toEqual([{ name: "lib", base_ref: "main" }]);
});

test("with no registered repos, the order form says how to register one (#211)", async ({ page }) => {
  bridge = new Bridge();
  await page.goto(await bridge.start({ repos: [] }));
  await expect(page.getByText("This bridge has no repos")).toContainText("--repo <name>=<url>");
  await expect(page.getByRole("button", { name: "Engage" })).toBeDisabled();
  await expect(page.getByLabel("Repo", { exact: true })).toHaveCount(0);
  await expect(page.getByLabel("Base ref")).toHaveCount(0);
});

test.describe("with notifications granted", () => {
  test.use({ permissions: ["notifications"] });

  test("a hail opened live raises one notification; one already open doesn't", async ({ page }) => {
    const errors: string[] = [];
    page.on("console", (m) => {
      if (m.type() === "error") errors.push(m.text());
    });
    page.on("pageerror", (e) => errors.push(e.message));
    // Headless Chromium shows no OS notifications: record them instead. The
    // headless shell also reports Notification.permission "denied" even when
    // the context grants it, so the stand-in reports the grant itself.
    await page.addInitScript(() => {
      const notes: FakeNote[] = [];
      window.__notes = notes;
      class Fake implements FakeNote {
        static readonly permission = "granted";
        static requestPermission() {
          return Promise.resolve("granted");
        }
        title: string;
        body: string;
        tag: string;
        closed = false;
        onclick: (() => void) | null = null;
        constructor(title: string, opts: NotificationOptions = {}) {
          this.title = title;
          this.body = opts.body ?? "";
          this.tag = opts.tag ?? "";
          notes.push(this);
        }
        close() {
          this.closed = true;
        }
      }
      Object.defineProperty(window, "Notification", { value: Fake, configurable: true, writable: true });
    });

    bridge = new Bridge();
    const url = await bridge.start();
    await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--division", "science");
    await bridge.bmus("crew", "add", "tovan", "--name", "Ens. Tovan", "--division", "science");

    // Sorell's hail is open before the page loads: it comes in the snapshot.
    const first = await bridge.order("sorell", "Survey the webhook sender");
    const firstHail = await bridge.openHail(first);
    await page.goto(url);
    await expect(page.locator(`#hail-${firstHail}`)).toBeVisible();
    const header = page.getByRole("banner");
    await expect(header).toContainText("HAIL ALERTS");
    await expect(header).not.toContainText("Blocked");
    await expect(header.getByRole("button", { name: "Notify me" })).toHaveCount(0);

    // Tovan's hail opens while the page is live.
    const second = await bridge.order("tovan", "Add retries to the webhook sender");
    const secondHail = await bridge.openHail(second);
    const card = page.locator(`#hail-${secondHail}`);
    await expect(card).toBeVisible({ timeout: missionTimeout });
    const notes = () => page.evaluate(() => (window.__notes ?? []).map((n) => ({ title: n.title, body: n.body, tag: n.tag, closed: n.closed })));
    await expect.poll(notes).toHaveLength(1);
    expect(await notes()).toEqual([{ title: `Hail from Ens. Tovan · ${second}`, body: "Run <b>x</b> make deploy?", tag: secondHail, closed: false }]);

    // Clicking it focuses the hail's card and closes it.
    await page.locator("body").click({ position: { x: 1, y: 1 } });
    await expect(card).not.toBeFocused();
    await page.evaluate(() => window.__notes?.[0]?.onclick?.());
    await expect(card).toBeFocused();
    expect((await notes())[0]?.closed).toBe(true);
    expect(await notes()).toHaveLength(1);
    expect(errors).toEqual([]);
  });
});

/** Collects the page's console errors, but a refused action's "Failed to load resource" (the test asks for those). */
function pageErrors(page: Page): string[] {
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() === "error" && !/^Failed to load resource: the server responded with a status of 4\d\d/.test(m.text())) errors.push(m.text());
  });
  page.on("pageerror", (e) => errors.push(e.message));
  return errors;
}

test("Phase 2 controls: standing orders, withdraw, route, belay and recall", async ({ page }) => {
  const errors = pageErrors(page);
  bridge = new Bridge();
  const url = await bridge.start();
  await page.goto(url);
  await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--division", "science");
  await bridge.bmus("crew", "add", "tovan", "--name", "Ens. Tovan", "--division", "science");
  const sorell = page.getByRole("article", { name: "Lt. Sorell" });
  const tovan = page.getByRole("article", { name: "Ens. Tovan" });
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });
  await expect(tovan).toContainText("On deck", { timeout: missionTimeout });

  // Standing orders: the switch shows the bridge's value and flips it.
  const standing = page.getByRole("switch", { name: "Standing orders" });
  await expect(standing).toHaveAttribute("aria-checked", "false");
  await expect(standing).toContainText("off");
  await standing.click();
  await expect(standing).toHaveAttribute("aria-checked", "true");
  await expect(standing).toContainText("on");
  expect(await bridge.bmus("standing-orders")).toBe("on\n");
  await standing.click();
  await expect(standing).toHaveAttribute("aria-checked", "false");
  expect(await bridge.bmus("standing-orders")).toBe("off\n");

  const open = page.getByRole("region", { name: /^Open orders/ });
  const away = page.getByRole("region", { name: /^Away missions/ });
  const record = page.getByRole("region", { name: "Mission record" });
  const recordRow = (id: string) => record.locator(".record-row").filter({ hasText: id });

  // Withdraw a pool order, after a confirm step that Escape cancels.
  const withdrawn = await bridge.createOrder({ title: "Survey the old relay", division: "science" });
  const withdrawnRow = open.locator(".row").filter({ hasText: withdrawn });
  await expect(withdrawnRow).toContainText("In the pool");
  const ask = withdrawnRow.getByRole("button", { name: `Withdraw ${withdrawn}` });
  await ask.click();
  const confirm = withdrawnRow.getByRole("group", { name: `Withdraw ${withdrawn}? Nobody will take it.` });
  await expect(confirm.getByRole("button", { name: "Withdraw" })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(confirm).toBeHidden();
  await expect(ask).toBeFocused();
  await ask.click();
  await confirm.getByRole("button", { name: "Withdraw" }).click();
  await expect(withdrawnRow).toBeHidden();
  await expect(recordRow(withdrawn)).toContainText("Withdrawn");

  // Route a pool order to Sorell, who takes it away and hails.
  const belayed = await bridge.createOrder({ title: "Recalibrate the deflector", division: "science" });
  const routeRow = open.locator(".row").filter({ hasText: belayed });
  await expect(routeRow.getByLabel("Route to")).toHaveValue("pool");
  await routeRow.getByLabel("Route to").selectOption("sorell");
  await routeRow.getByRole("button", { name: `Route ${belayed}` }).click();
  await expect(routeRow).toBeHidden({ timeout: missionTimeout });
  await expect(away.locator(".row").filter({ hasText: belayed })).toContainText("SORELL", { timeout: missionTimeout });

  // Belay it from its hail.
  const hails = page.getByRole("region", { name: "Hailing frequencies open" });
  const card = hails.locator(".row.hail").filter({ hasText: "Recalibrate the deflector" });
  await expect(card).toBeVisible({ timeout: missionTimeout });
  await card.getByRole("button", { name: `Belay order ${belayed}` }).click();
  await card.getByRole("group", { name: `Belay ${belayed}? The order is abandoned.` }).getByRole("button", { name: "Belay" }).click();
  await expect(hails).toBeHidden({ timeout: missionTimeout });
  await expect(recordRow(belayed)).toContainText("Belayed", { timeout: missionTimeout });
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });

  // Beam Tovan up: the order goes back to the pool from its checkpoint.
  const recalled = await bridge.createOrder({ title: "Chart the nebula", assignee: "tovan", division: "science" });
  const awayRow = away.locator(".row").filter({ hasText: recalled });
  await expect(awayRow).toBeVisible({ timeout: missionTimeout });
  await awayRow.getByRole("button", { name: `Beam TOVAN up and return ${recalled} to the pool` }).click();
  await awayRow
    .getByRole("group", { name: `Beam TOVAN up? ${recalled} goes back to the pool from its last checkpoint.` })
    .getByRole("button", { name: "Beam up" })
    .click();
  const recalledRow = open.locator(".row").filter({ hasText: recalled });
  await expect(recalledRow).toContainText("In the pool", { timeout: missionTimeout });
  await expect(awayRow).toBeHidden();
  await expect(tovan).toContainText("On deck", { timeout: missionTimeout });

  // A refusal shows inline: a recalled order is parked, and withdraw sends it to belay.
  await recalledRow.getByRole("button", { name: `Withdraw ${recalled}` }).click();
  await recalledRow.getByRole("group").getByRole("button", { name: "Withdraw" }).click();
  await expect(recalledRow.getByRole("alert")).toContainText(`order ${recalled} is parked; use belay`);
  await expect(recalledRow.getByRole("button", { name: `Withdraw ${recalled}` })).toBeFocused();

  // After a reload the record still lists the withdrawn and belayed orders.
  await page.reload();
  await expect(recordRow(withdrawn)).toContainText("Withdrawn");
  await expect(recordRow(belayed)).toContainText("Belayed");

  // Standing orders on: a free crew member of science claims the recalled order.
  await expect(standing).toHaveAttribute("aria-checked", "false");
  await standing.click();
  await expect(standing).toHaveAttribute("aria-checked", "true");
  await expect(away.locator(".row").filter({ hasText: recalled })).toBeVisible({ timeout: missionTimeout });
  await expect(open.getByText("No open orders. The pool is clear.")).toBeVisible();
  expect(errors).toEqual([]);
});

test("re-energize brings a crew member in sickbay back on deck", async ({ page }) => {
  const errors = pageErrors(page);
  bridge = new Bridge();
  const url = await bridge.start();
  await page.goto(url);
  await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--division", "science");
  const sorell = page.getByRole("article", { name: "Lt. Sorell" });
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });
  const beam = sorell.locator(".crew-foot .muted-id");
  await expect(beam).toHaveText(/^BEAM holodeck-/, { timeout: missionTimeout });
  const first = (await beam.innerText()).trim();

  // The beam dies: its heartbeats stop, and the pump sends Sorell to
  // sickbay after its 30s staleness window.
  bridge.killBeams();
  await expect(sorell).toContainText("Sickbay", { timeout: 60_000 });
  await sorell.getByRole("button", { name: "Re-energize beam for Lt. Sorell" }).click();
  await expect(sorell).toContainText("On deck", { timeout: missionTimeout });
  await expect(beam).not.toHaveText(first);
  expect(errors).toEqual([]);
});

test("countermand a captain timeout's default option inside its window", async ({ page }) => {
  const errors = pageErrors(page);
  bridge = new Bridge();
  // No rules and no Command: the hail goes to the captain, whose 1s
  // timeout takes option 0 with a 10m countermand window.
  const url = await bridge.start({
    regs: "command_may_answer:\n  countermand_window: 10m\nescalation:\n  on_timeout: default_option\n  captain_timeout: 1s\n  default_option: 0\n",
    script: countermandScript,
  });
  await page.goto(url);
  await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--division", "science");
  await expect(page.getByRole("article", { name: "Lt. Sorell" })).toContainText("On deck", { timeout: missionTimeout });
  const orderId = await bridge.order("sorell", "Deploy the relay");

  const decided = page.getByRole("region", { name: "Decided without you" });
  const card = decided.locator(".row").filter({ hasText: "Deploy the relay" });
  await expect(card).toContainText("Decided by the captain timeout: allow", { timeout: missionTimeout });
  await expect(card).toContainText("Countermand until");

  // The mission finishes meanwhile: the card says the agent won't be told.
  await expect(card).toContainText(`No live run for ${orderId}`, { timeout: missionTimeout });
  await card.getByLabel(/^Reason for countermanding H-/).fill("staging is frozen");
  await card.getByRole("button", { name: /^Countermand H-\d+ with deny$/ }).click();
  const confirm = card.getByRole("group", { name: /^Replace "allow" with "deny"\?/ });
  await expect(confirm.getByRole("button", { name: "Countermand" })).toBeFocused();
  await confirm.getByRole("button", { name: "Countermand" }).click();

  // decided_by changes through the feed; the window still runs, so the
  // card stays, now offering allow back.
  await expect(card).toContainText("Decided by you (countermanded): deny", { timeout: missionTimeout });
  await expect(card.getByRole("status").filter({ hasText: "Countermanded." })).toHaveText(
    `Countermanded. order ${orderId} is done: no live run, the agent won't be told.`,
  );
  await expect(card.getByRole("button", { name: /^Countermand H-\d+ with allow$/ })).toBeVisible();
  const { hails } = JSON.parse(await bridge.bmus("hails", "list", "--status", "decided", "--json")) as {
    hails: { order_id: string; decided_by: string; original_decided_by: string; decision: string; rationale: string }[];
  };
  expect(hails.find((h) => h.order_id === orderId)).toMatchObject({
    decided_by: "captain", original_decided_by: "policy", decision: "deny", rationale: "staging is frozen",
  });
  expect(errors).toEqual([]);
});

test("Command's split, approved from the page, waits on its open children", async ({ page }) => {
  const errors = pageErrors(page);
  bridge = new Bridge();
  const url = await bridge.start({ script: splitScript });
  await page.goto(url);
  await bridge.bmus("crew", "add", "vance", "--name", "Adm. Vance", "--division", "command");
  await expect(page.getByRole("article", { name: "Adm. Vance" })).toContainText("On deck", { timeout: missionTimeout });
  const parent = await bridge.order("vance", "Split the relay RFC");

  const hails = page.getByRole("region", { name: "Hailing frequencies open" });
  // The bridge's summary of what it will create, not the agent's question.
  await expect(hails.getByText(`"Split ${parent} into 2 orders: 1) Relay API [science, routine]; 2) Relay UI [science, routine]"`, { exact: true }))
    .toBeVisible({ timeout: missionTimeout });
  await expect(hails.getByText("the agent's own words")).toHaveCount(0);
  await hails.getByRole("button", { name: "allow", exact: true }).click();
  await expect(hails).toBeHidden({ timeout: missionTimeout });

  // The children wait in the science pool, which nobody serves; the
  // parent's mission is over and it waits on them.
  const record = page.getByRole("region", { name: "Mission record" });
  const row = record.locator(".record-row").filter({ hasText: parent });
  await expect(row).toContainText("Split: waiting on 2 children", { timeout: missionTimeout });
  await expect(row).toContainText("Split the relay RFC");
  const { orders } = JSON.parse(await bridge.bmus("order", "list", "--json")) as {
    orders: { id: string; parent_id: string; state: string; title: string; open_children: number }[];
  };
  const kids = orders.filter((o) => o.parent_id === parent);
  expect(kids.map((o) => [o.title, o.state])).toEqual([["Relay API", "pool"], ["Relay UI", "pool"]]);
  expect(orders.find((o) => o.id === parent)).toMatchObject({ state: "split", open_children: 2 });
  await expect(page.getByRole("article", { name: "Adm. Vance" })).toContainText("On deck", { timeout: missionTimeout });

  // Each child that ends updates the count through the feed; the last
  // settles the parent, failed: withdrawn children aren't done.
  await bridge.bmus("order", "withdraw", kids[0]?.id ?? "");
  await expect(row).toContainText("Split: waiting on 1 child", { timeout: missionTimeout });
  await bridge.bmus("order", "withdraw", kids[1]?.id ?? "");
  await expect(row).toContainText("Failed", { timeout: missionTimeout });
  expect(errors).toEqual([]);
});

test("belaying a split order from the page withdraws its open children", async ({ page }) => {
  const errors = pageErrors(page);
  bridge = new Bridge();
  const url = await bridge.start({ script: splitScript });
  await page.goto(url);
  await bridge.bmus("crew", "add", "vance", "--name", "Adm. Vance", "--division", "command");
  await expect(page.getByRole("article", { name: "Adm. Vance" })).toContainText("On deck", { timeout: missionTimeout });
  const parent = await bridge.order("vance", "Split the relay RFC");
  const hails = page.getByRole("region", { name: "Hailing frequencies open" });
  await hails.getByRole("button", { name: "allow", exact: true }).click({ timeout: missionTimeout });

  const record = page.getByRole("region", { name: "Mission record" });
  const row = record.locator(".record-row").filter({ hasText: parent });
  await expect(row).toContainText("Split: waiting on 2 children", { timeout: missionTimeout });
  await row.getByRole("button", { name: `Belay ${parent} and its open child orders` }).click();
  const confirm = row.getByRole("group", { name: `Belays ${parent} and its 2 open child orders. Belay them all?` });
  await confirm.getByRole("button", { name: "Belay" }).click();
  await expect(row).toContainText("Belayed", { timeout: missionTimeout });
  const { orders } = JSON.parse(await bridge.bmus("order", "list", "--json")) as {
    orders: { id: string; parent_id: string; state: string }[];
  };
  expect(orders.filter((o) => o.parent_id === parent).map((o) => o.state)).toEqual(["withdrawn", "withdrawn"]);
  expect(orders.find((o) => o.id === parent)?.state).toBe("belayed");
  expect(errors).toEqual([]);
});

test("a done order's work is pushed from its record row, forced past a diverged target, and its mission report shown and its diff copied (#4, #104)", async ({ page, browser }) => {
  const errors = pageErrors(page);
  // Headless Chromium's clipboard needs a grant and a focused page: record
  // what the page writes instead. On the context, so the diff page's tab
  // has it too, with the CSP violations.
  await page.context().addInitScript(() => {
    (window as unknown as { __clip: string[] }).__clip = [];
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: (t: string) => ((window as unknown as { __clip: string[] }).__clip.push(t), Promise.resolve()) },
      configurable: true,
    });
    window.__csp = [];
    document.addEventListener("securitypolicyviolation", (e) => window.__csp?.push(`${e.violatedDirective} ${e.blockedURI}`));
  });
  bridge = new Bridge();
  const url = await bridge.start({ script: workScript });
  const bare = path.join(bridge.dir, "app.git");
  await page.goto(url);
  await bridge.bmus("crew", "add", "sorell", "--name", "Lt. Sorell", "--division", "science");
  await expect(page.getByRole("article", { name: "Lt. Sorell" })).toContainText("On deck", { timeout: missionTimeout });
  const orderId = await bridge.order("sorell", "Add retries");
  const branch = `bmus/${orderId}`;

  const record = page.getByRole("region", { name: "Mission record" });
  const row = record.locator(".record-row").filter({ hasText: orderId });
  await expect(row).toContainText("Complete", { timeout: missionTimeout });
  const clone = path.join(bridge.home, "repos", "app");
  const commit = await gitOut(bridge.dir, "--git-dir", clone, "rev-parse", branch);

  // Push: the confirm names the remote and the branch.
  await row.getByRole("button", { name: `Push ${orderId}'s work to its remote` }).click();
  const confirm = row.getByRole("group", { name: `Push ${branch} to ${bare} as ${branch}?` });
  await expect(confirm.getByLabel("Target branch")).toHaveValue(branch);
  await expect(confirm.getByRole("button", { name: "Push", exact: true })).toBeFocused();
  await confirm.getByRole("button", { name: "Push", exact: true }).click();
  const pushed = row.getByRole("status").filter({ hasText: "Pushed." });
  await expect(pushed).toContainText(`app: ${branch} at ${commit.slice(0, 12)}`, { timeout: missionTimeout });
  await expect(pushed.getByRole("link")).toHaveCount(0); // not a github.com remote
  expect(await gitOut(bridge.dir, "ls-remote", bare, branch)).toBe(`${commit}\trefs/heads/${branch}`);
  await expect(row).toContainText(`pushed to ${branch}`);
  await expect(row.getByRole("button", { name: "Force push" })).toHaveCount(0);

  // A diverged target: the rejection, then a separate force confirm.
  const main = await gitOut(bridge.dir, "--git-dir", bare, "rev-parse", "main");
  const tree = await gitOut(bridge.dir, "--git-dir", bare, "rev-parse", "main^{tree}");
  const diverged = (
    await execFileP("git", ["-c", "user.name=bmus e2e", "-c", "user.email=e2e@bmus.invalid", "--git-dir", bare, "commit-tree", "-p", main, "-m", "elsewhere", tree])
  ).stdout.trim();
  await gitOut(bridge.dir, "--git-dir", bare, "update-ref", "refs/heads/feature", diverged);
  await row.getByRole("button", { name: `Push ${orderId}'s work to its remote` }).click();
  await row.getByLabel("Target branch").fill("feature");
  await row.getByRole("group", { name: `Push ${branch} to ${bare} as feature?` }).getByRole("button", { name: "Push", exact: true }).click();
  await expect(row.getByRole("alert")).toContainText("The remote rejected the push: feature has likely diverged", { timeout: missionTimeout });
  expect(await gitOut(bridge.dir, "--git-dir", bare, "rev-parse", "feature")).toBe(diverged);
  await row.getByRole("button", { name: `Force push ${orderId} to feature` }).click();
  const force = row.getByRole("group", { name: `Force push ${branch} to ${bare} as feature? The remote branch's commits that ${branch} doesn't have are overwritten.` });
  await expect(force.getByRole("button", { name: "Force push" })).toBeFocused();
  await force.getByRole("button", { name: "Force push" }).click();
  await expect(row.getByRole("status").filter({ hasText: "Pushed." })).toContainText(`app: feature at ${commit.slice(0, 12)}`, { timeout: missionTimeout });
  expect(await gitOut(bridge.dir, "ls-remote", bare, "feature")).toBe(`${commit}\trefs/heads/feature`);
  await expect(row).toContainText("pushed to feature");

  // Mission report opens the order's report in a new tab: the crew
  // member, the beam, the summary, the changed file and its added line;
  // Copy puts the raw diff on the clipboard.
  const beamText = (await page.getByRole("article", { name: "Lt. Sorell" }).locator(".crew-foot .muted-id").textContent()) ?? "";
  const beam = /holodeck-\d+-\d+/.exec(beamText)?.[0] ?? "no beam";
  const popup = page.waitForEvent("popup");
  await row.getByRole("link", { name: `Open the mission report of ${orderId}` }).click();
  const diffPage = await popup;
  const diffErrors = pageErrors(diffPage);
  expect(new URL(diffPage.url()).pathname).toBe(`/report/${orderId}`);
  const added = `+${orderId} step 1: add retries to the webhook sender`;
  await expect(diffPage.getByRole("heading", { level: 1 })).toHaveText("MISSION REPORT");
  await expect(diffPage.locator(".report-order")).toHaveText(`${orderId} Add retries`);
  const crewPanel = diffPage.getByRole("region", { name: "Crew" });
  await expect(crewPanel).toContainText("Lt. Sorell");
  await expect(diffPage.getByRole("region", { name: "Beam" }).getByText(beam, { exact: true })).toBeVisible();
  await expect(diffPage.getByRole("region", { name: "Report" })).toContainText("retries committed");
  await expect(diffPage.getByRole("region", { name: "Pushes" })).toContainText(`app: feature at ${commit.slice(0, 12)}`);
  await expect(diffPage.getByRole("heading", { name: "holodeck.txt" })).toBeVisible();
  await expect(diffPage.getByText(added, { exact: true })).toBeVisible();
  await expect(diffPage.getByText(added, { exact: true })).toHaveClass("dl dl-add");
  await diffPage.getByRole("button", { name: `Copy the diff of ${orderId}` }).click();
  await expect(diffPage.getByRole("status").filter({ hasText: "Copied." })).toBeVisible();
  const [clip] = await diffPage.evaluate(() => (window as unknown as { __clip: string[] }).__clip);
  expect(clip).toMatch(/^diff --git a\/holodeck.txt b\/holodeck.txt\n/);
  expect(clip).toContain(`\n${added}\n`);
  expect(await diffPage.evaluate(() => window.__csp)).toEqual([]);
  expect(diffErrors).toEqual([]);

  // Without a session the page says so, and shows no diff.
  const fresh = await browser.newContext();
  const stranger = await fresh.newPage();
  await stranger.goto(diffPage.url());
  await expect(stranger.getByText("No session. Open the console with `bmus console`.")).toBeVisible();
  await expect(stranger.getByText(added)).toHaveCount(0);
  await fresh.close();
  expect(errors).toEqual([]);
});
