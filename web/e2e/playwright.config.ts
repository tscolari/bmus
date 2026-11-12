import { defineConfig, devices } from "@playwright/test";

// BMUS_E2E_CHROMIUM: a Chromium executable to use instead of the one matching @playwright/test (e.g. a preinstalled one).
const chromium = process.env["BMUS_E2E_CHROMIUM"];

export default defineConfig({
  testDir: ".",
  globalSetup: "./global-setup.ts",
  outputDir: "test-results",
  // One bridge per test: run them one at a time.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env["CI"],
  retries: 0,
  timeout: 180_000,
  expect: { timeout: 15_000 },
  reporter: [["list"], ["html", { outputFolder: "playwright-report", open: "never" }]],
  use: {
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"], ...(chromium ? { launchOptions: { executablePath: chromium } } : {}) },
    },
  ],
});
