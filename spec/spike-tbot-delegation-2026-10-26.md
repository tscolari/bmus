# Spike: delegated identities for the bridge

Status: run (2026-10-26, #84). Follows `spike-tbot-bridge-identity-2026-10-18.md`.

## Why

The first spike left question 2 open: a beam owned by a Machine ID bot gets no inference. A separate hackathon project reported a bot creating beams "as the user who directed it". This spike checks how, on the same cluster, reusing the `bmus-bridge` role and bot.

## Role impersonation: dead end

Granting the bot `allow.impersonate` on the captain and running `tctl auth sign --user=<captain>` from the bot's identity fails twice:

1. `identity is not allowed to reissue certificates`. Bot certs carry `disallow-reissue` by default; rejoining with `tbot start identity --allow-reissue` clears it.
2. `impersonated user can not impersonate anyone else`. Every bot cert carries an `impersonator` extension from the moment it joins, and Teleport refuses recursive impersonation. The request is rejected on the caller's identity before the role's grant is evaluated, so no role change helps.

The role was put back to its original, impersonate-free spec.

## Delegation sessions: question 2 resolved

Teleport has a separate API built for this, `teleport.delegation.v1.DelegationSessionService`. A human grants a bot the right to act as them, time-boxed. The service ships in Teleport v18.11.1.

The first spike missed it because its client (18.10.0) has no `delegation` command. The proxy was already at 18.11.4; a downloaded 18.11.4 client (self-updated to 18.11.7) exposes `tsh delegation create-session`.

What was tested, end to end:

1. As the captain: `tsh delegation create-session --proxy=example.beams.sh:443 --user=captain@example.com --bot=bmus-bridge --allow-all --session-ttl=30m` printed a session id. No MFA prompt appeared. The docs say the call needs MFA, and an existing MFA grace window probably covered it; this is unconfirmed. The command also takes `--allow-node`, `--allow-app`, `--allow-db` and similar flags for a scoped grant. Only `--allow-all` was tried.
2. The bot's half has no `tsh` verb. A throwaway Go program (about 90 lines, not committed) loaded the bot's tbot identity as mTLS credentials and called `DelegationSessionServiceClient().GenerateCerts` with the session id, fresh SSH and TLS keys and a 15-minute TTL, through Teleport's `api` module (Apache-2.0). It wrote the result as an identity file, reusing the bot's CA certs the way the plugin's `generateDelegatedIdentity` does.
3. `tsh status -i <file>` showed `Logged in as: captain@example.com` with the captain's roles (`access, auditor, beam-user, editor`), the `bot-name` and `delegation-session-id` extensions, and a `Delegation session:` line.
4. `tsh -i <file> beams add --no-console --format=json` created a beam owned by the captain.
5. `claude -p "say ok"` inside it answered. Bot-owned beams got a 302 to the web launcher in the first spike.
6. A second `GenerateCerts` call with the same session id returned another valid cert. One session (up to 7 days, created by a human) backs any number of certs (up to 1h each) with no further interaction.

There is no command to list or revoke a session; `tsh delegation` only has `create-session`. Both test sessions were left to lapse (10 to 30 minute TTLs) and the test beam was removed.

## Outcome

Against the first spike's table: steps pass with a workaround. The workaround is bigger than the table assumed. The bridge would not run as a bot: it would run as the captain, through certs it mints itself.

- A delegated cert carries all of the captain's roles. With `--allow-all` it gives up the least-privilege goal that motivated #84. A Beams-only scope is untested.
- The credential on the laptop lasts up to 7 days and nothing can revoke it, against a captain's `tsh` login that lasts hours.
- The captain still runs `create-session` by hand, at most every 7 days, instead of `tsh login` daily. D8's expiry moves; it doesn't go away.
- The bridge's machine needs `tsh` and `tbot` 18.11.1 or later.
- Minting needs the `api` module or a tbot output that does it. The tsh transporter itself can stay as it is and pass `-i <minted file>`. The native API transporter stays in Phase 4.

## Decision

The bridge keeps the captain's `tsh` session, with the #102 login gate. Delegation isn't built now.

I think the trade is wrong today. A 7-day, unrevocable credential with the captain's full roles is worse than the daily login it replaces. tbot may also gain direct Beams support, which would make the bot's own identity work and the minting code throwaway.

Revisit when either holds:

- tbot can own beams that get inference. The first spike's `--tsh-identity` plan then applies as written.
- A scoped delegation grant (Beams only) is shown to still reach inference, and sessions can be listed or revoked.

The `bmus-bridge` role and bot are still on the cluster for that re-run.
