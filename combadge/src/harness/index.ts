import { echoHarness } from "./echo.js";
import { holodeckHarness } from "./holodeck.js";
import type { HarnessFactory } from "./types.js";

/**
 * Harnesses `serve --harness` accepts, built from serve's flags. sdk is
 * imported only when chosen: evaluating the inlined Agent SDK costs serve and
 * attach startup time they don't need otherwise.
 */
export const harnesses: Record<string, HarnessFactory | undefined> = {
  echo: echoHarness,
  holodeck: holodeckHarness,
  sdk: async (opts) => (await import("./sdk.js")).sdkHarness(opts),
};
