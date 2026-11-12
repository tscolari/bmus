# Phase 2 plan amendment: login expiry, idle wake-up and provisioning retries

Three changes to `phase-2-plan-2026-10-19.md`, tracked in #102, #96 and #97. The exit criterion has six crew running for a full day, and each change fixes a way that day goes wrong without anyone noticing.

## 1. The captain's tsh login expires mid-run (new issue)

Every `tsh` call runs on the captain's login (D8). The tbot spike settled that the login stays (`spike-tbot-bridge-identity-2026-10-18.md`). A day-long run will outlive it. Today the bridge doesn't notice:

- The sickbay check treats a failed probe as "health unknown" and keeps the crew member on deck. Attaches keep failing, and `bmus status` stays green while nothing moves.
- A watch change runs `beams add`. A failed provision sends the crew member to sickbay, so every rotation after the expiry costs a crew member.

The change:

- `transporter.Tsh` gains `LoginValidUntil(ctx) (time.Time, error)`, read from `tsh status --format=json`. Holodeck returns the zero time, meaning no expiry.
- The daemon checks it every minute and keeps the result. `bridge.status` adds a yellow reason, `tsh login expires in <n>m`, inside the last 30 minutes, and `tsh login expired` after expiry. This is the D8 re-login prompt, and the console header shows it through the `status` event.
- `provision` waits while the login is expired. It doesn't count those minutes against its retry budget (§3) and logs `provisioning waits for a tsh login` once. Re-logging in resumes it, and nobody has to re-energize anyone.

What `tsh` does with an expired login (an error, a browser prompt, a hang) is the open #90 check. The design assumes an error or a hang bounded by the call's context. If it turns out to open a browser, `Tsh.run` needs a flag or env var that stops that. #90 should run before this issue starts.

## 2. Idle tours re-check for work every 10 minutes (#96)

`wakeDivision` is best effort. A wake that never arrives (a failed send, or a tour mid-restart) leaves a pool order waiting until the idle tour's `Recv` times out. That's `crewTourWaitTimeout`, 24h today, and only the watch-change cap (about 23h) would shorten it. The idle wait becomes `missionCheckEvery` (10 minutes), the same as a busy tour's. A claim that finds nothing costs one query and writes nothing.

## 3. Provisioning retries before sickbay (#97)

The beam cap counts the crew. The tenant's 20-beam cap counts every beam the captain owns, throwaway beams included, and the 5-per-minute rate limit counts creations from anywhere. A `beams add` that fails on either sends the crew member to sickbay, the same as a broken provider would.

`provision` retries a failed `beams add` with backoff (1, 2, 4 ... minutes, capped at 5) for up to 30 minutes, then fails as today. I'd rather not match on tsh's error text for "cap" or "rate": nobody has seen the exact strings, and a retry costs a few minutes in the cases that turn out permanent. A provision waiting on the login (§1) doesn't use up this budget.
