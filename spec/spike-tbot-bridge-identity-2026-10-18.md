# Spike: tbot as the bridge's Teleport identity

Status: run (2026-10-19, #84). Findings appended below.

## Why

The tsh transporter runs every `tsh` call on the captain's login session (D8 in `initial-plan-2026-10-16.md`). That session expires, and the bridge then stalls until the captain logs in again. The plan's answer is a yellow condition and a re-login prompt, which is fine at a desk and bad for an always-on bridge.

tbot (Machine ID) keeps a bot's certificates renewed with no human in the loop. A bot also gives the bridge its own low-privilege identity, which the plan already asks for while delegation inherits the user's full permissions ("Run the bridge as a low-privilege Teleport user until permissions can be scoped per beam").

If the spike passes, the code change is small: a `--tsh-identity <path>` flag on `bmus bridge run` that makes the tsh transporter (`internal/transporter/tsh.go`) pass `-i <path>` to every `tsh` call. The default stays the captain's session.

What we don't know is whether Beams works for a bot at all.

## Questions

1. Can a bot identity create, list, exec into, scp to and from, and remove beams? Who owns a beam it creates?
2. Does a bot-owned beam get inference through the LLM proxy, and who is billed for it?
3. Does `tsh beams` honour `-i <identity>`, or does it need a different tbot output?
4. Does the identity keep working past one certificate TTL with no interaction?
5. Which join method works from a laptop? Bound keypair first, a one-time token as the fallback.
6. What is the smallest role that still runs a beam end to end, and what does the audit log show as the actor?

## Prerequisites

- A Teleport cluster with Beams enabled, and rights to create roles, bots and join tokens (`tctl`).
- `tsh` 18.8.0 or later and a matching `tbot` on the machine that will run the bridge.
- The bmus repo built (`go build ./cmd/bmus`, and `cd combadge && npm ci && npm run build`).

Commands below are a starting point. tbot's flags have moved between releases. Check `tbot --help` and the cluster's docs for the installed version, and record the exact commands that worked.

## Steps

Work in a scratch directory. Keep every command and its output; the findings need them.

### 1. Role and bot

Write a role `bmus-bridge` that allows Beams and nothing else: no node, database, kube or app access. The Beams docs name the resources and verbs; record which ones were needed. Then:

```
tctl create -f bmus-bridge-role.yaml
tctl bots add bmus-bridge --roles=bmus-bridge
```

Create a bound-keypair join token for the bot. If bound keypair isn't available on the cluster, use the one-time token `tctl bots add` prints and note it.

### 2. Run tbot

```
install -d -m 0700 ~/.bmus/tbot
tbot start identity \
  --proxy-server=<proxy>:443 \
  --join-method=bound_keypair --token=<token> \
  --destination=~/.bmus/tbot \
  --storage=~/.bmus/tbot-state
```

Pass: tbot stays up and writes an `identity` file under `~/.bmus/tbot`. Record the cert TTL and renewal interval it reports.

### 3. Beams as the bot

Run each with no captain session present (`tsh logout` first, or a clean `TELEPORT_HOME`):

```
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams add --no-console --format=json
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams ls --format=json
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams exec <beam> -- echo ok
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams scp ./file <beam>:/tmp/file
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams scp <beam>:/tmp/file ./file.back
printf 'line\n' | tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams exec <beam> -- cat
tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 beams rm <beam>
```

Pass: every command succeeds, and the piped `cat` prints `line` and exits (stdin EOF reaches the beam; the pump's graceful recycle depends on it). Record the owner `beams ls` shows and whether the captain's own `tsh beams ls` can see the beam.

If `-i` is rejected or ignored, try tbot's other outputs before calling it a failure: an identity output with a generated `tsh` profile (`TELEPORT_HOME` pointing at the destination), then an application tunnel. Record which one worked.

### 4. Inference inside a bot-owned beam

Create a beam as the bot, then inside it:

```
env | grep -i anthropic
claude -p "say ok"
```

Pass: `ANTHROPIC_BASE_URL` points at the LLM proxy and `claude` answers. Record who the usage is billed to, from the cluster's usage view or the proxy's audit events.

### 5. The bridge on the bot

Run `tsh` through the identity by putting a two-line wrapper first on `PATH` (the bridge has no flag for it yet):

```
#!/bin/sh
exec /usr/local/bin/tsh -i ~/.bmus/tbot/identity --proxy=<proxy>:443 "$@"
```

Then `bmus bridge run --transporter tsh`, add a crew member, issue an order against a small test repo, and let it finish.

Pass: the order completes, its work lands in `~/.bmus/repos/<name>` under `bmus/<order>`, and `bmus status` stays green.

### 6. Renewal

Leave tbot and the bridge running for longer than one cert TTL (shorten the TTL in the bot's config to make this quick). Issue a second order after the original cert has expired.

Pass: it completes with nobody touching the machine.

### 7. Audit and least privilege

- Check the audit log for the beam events from steps 3 to 5. Record the actor shown.
- With the bot's identity, try something the role should forbid, for example `tsh -i ... ls` against nodes or `tsh -i ... db ls`. Pass: denied.
- Try removing a beam the captain owns. Record the result.

## What to do with the findings

Append a `## Findings` section to this file with the date, versions (`tsh version`, `tbot version`, cluster version), the role YAML, the exact commands that worked, and pass or fail for each step. Then pick the row that matches:

| Outcome | Next |
|---|---|
| Steps 1 to 7 pass | Open an issue to add `--tsh-identity` to `bmus bridge run` and the tsh transporter, with the step 5 run as its acceptance. Add a dated decision to `spec/` that the bridge runs as a tbot identity when given one, and that D8's re-login prompt only applies to the captain's own session. |
| Steps pass only with a workaround (a profile instead of `-i`, a tunnel, a token join) | Same as above, with the workaround as the flag's contract. List each workaround in the decision. |
| A bot can't own beams, or a bot-owned beam has no inference | Record the blocker here and in the tracking issue, and keep the captain's session. Report it upstream; the plan's low-privilege user stays a plain Teleport user until then. |
| Renewal fails (step 6) | Record how it failed. The flag can still land for the least-privilege win, and the D8 yellow condition stays. |

In every case, comment on the tracking issue with the outcome row and a link to the findings. Don't write code in the spike; the follow-up issue carries it.

The trade-offs to weigh once it passes, for the decision record:

- A standing, renewable credential lives on the captain's laptop. I think that's acceptable with a beams-only role, a short cert TTL and the tbot directories at 0700 inside the bridge home, and better than the captain's full-permission session doing the same job.
- Audit entries name the bot. Orders and the captain's log still tie each beam to a crew member and order.
- Git push doesn't change. Work comes back as bundles and the captain pushes with their own credentials (D6).

## Findings (2026-10-19)

Status: run. Cluster `example.beams.sh:443`, user `captain@example.com`. `tsh`/`tctl`/`tbot` 18.10.0 (`go1.25.14`), proxy 18.11.4. Scratch work in `/tmp/bmus-spike-84`, `BMUS_HOME=~/.bmus-spike84` (the already-running production bridge on `~/.bmus` was left untouched throughout).

**Outcome row: "A bot can't own beams, or a bot-owned beam has no inference."** A bot identity owns and drives beams end to end (question 1: pass) but a bot-owned beam gets no inference through the LLM proxy (question 2: fail), which is the hard blocker. Recommendation: report this upstream; keep the captain's `tsh` session as the tsh transporter's identity until the LLM proxy accepts bot-owned beams.

### Role used

```yaml
kind: role
version: v8
metadata:
  name: bmus-bridge
spec:
  allow:
    logins:
      - beams
    node_labels:
      teleport.internal/beams/owner: '{{user.metadata.name}}'
    beam_labels:
      teleport.internal/beams/owner: '{{user.metadata.name}}'
    app_labels_expression: |
      labels["teleport.internal/beams/app-type"] == "llm" ||
      labels["teleport.internal/beams/owner"] == user.metadata.name
    rules:
      - resources: [beam]
        verbs: ["*"]
      - resources: [beams_config]
        verbs: [list, read]
```

This is the cluster's own `beam-user` preset verbatim, renamed. No smaller role was tried, since `beam-user` already has nothing beyond Beams, and question 2's failure sits below the role layer (see step 4) — a smaller role wouldn't change it. `tsh -i <bot> ls` (question 6's "what does least privilege actually forbid") shows one node: the bot's own beam, not a general node listing — `node_labels` grants exec/scp into the beam-as-node and nothing wider. `tsh -i <bot> db ls` is empty.

### Step-by-step

**1. Role and bot — pass, with the token fallback.** `tctl get role/beam-user` matched the plan's assumption exactly; `tctl create -f bmus-bridge-role.yaml` succeeded; no pre-existing `bmus-bridge` role or bot (checked first). `tctl bots add bmus-bridge --roles=bmus-bridge --ttl=2h` printed a plain one-time token (`tctl bots add --help` has no `--join-method` flag at all on this tsh/tctl version), not a `tbot+proxy+bound-keypair://` URI — despite `tctl status` showing the `bound_keypair` CA active. **Question 5 resolves to the token fallback**, not bound keypair: an active bound-keypair CA doesn't mean `tctl bots add`'s simple flow issues a bound-keypair join; that path (if it exists on 18.10.0) needs a different command than the one the plan and the bot-add help text point at.

**2. Run tbot — pass.** `tbot start identity --token=<token> --join-method=token --proxy-server=example.beams.sh:443 --destination=file://~/.bmus-spike84/tbot --storage=file://~/.bmus-spike84/tbot-state` joined on the first try. Default cert TTL 1h, renewal interval 20m (tbot logs both at startup). `tsh status -i ~/.bmus-spike84/tbot/identity --proxy=example.beams.sh:443` showed `Logged in as: bot-bmus-bridge`, role `bmus-bridge`, valid for 59m. `--destination`/`--storage` do need `file://` + absolute path, as the plan expected.

**3. Beams as the bot — pass.** With `TELEPORT_HOME` unset to a scratch dir and only `-i`/`--proxy` set, every command worked with no `-i` rejection or fallback needed:

```
tsh -i $IDENT --proxy=$PROXY beams add --no-console --format=json
tsh -i $IDENT --proxy=$PROXY beams ls --format=json
tsh -i $IDENT --proxy=$PROXY beams exec <beam> -- echo ok
tsh -i $IDENT --proxy=$PROXY beams scp ./file <beam>:/tmp/file
tsh -i $IDENT --proxy=$PROXY beams scp <beam>:/tmp/file ./file.back
printf 'line\n' | tsh -i $IDENT --proxy=$PROXY beams exec <beam> -- cat
tsh -i $IDENT --proxy=$PROXY beams rm <beam>
```

`diff` on the scp round-trip was empty; the piped `cat` printed exactly `line` and exited (stdin EOF reaches the beam, which `internal/comms/pump.go`'s recycle depends on). `beams add --format=json`'s actual shape is `{id, uuid, owner, expires}` — `owner` was `bot-bmus-bridge` for every beam the bot created, confirming `tshBeam`'s `{id, expires}` subset in `internal/transporter/tsh.go:39-42` (TODO #90) already covers what the bridge needs; the extra `uuid`/`owner` fields are harmlessly ignored by Go's JSON decode.

Cross-ownership, checked both directions: the captain's own `tsh beams ls` and `tsh beams ls --all` both showed only the captain's own beam, never the bot's — `--all` needs a `beam-admin`-shaped grant (`beam_labels: {owner: '*'}`) the captain's roles (`access, auditor, beam-user, editor`) don't have, so it didn't broaden anything here. The captain's `tsh beams rm <bot's beam>` was denied ("access to beam denied"); the bot's `tsh -i <bot> beams rm <captain's beam>` (step 7) was denied the same way. Ownership is exclusive in both directions.

**4. Inference inside a bot-owned beam — fail. This is the blocker.** `env | grep -i anthropic` inside a bot-owned beam shows the same `ANTHROPIC_BASE_URL`/`ANTHROPIC_API_KEY` etc. as a captain-owned beam. `claude -p "say ok"` fails:

> API Error: API returned an empty or malformed response (HTTP 200) — check for a proxy or gateway intercepting the request. Response: content-type html, body is an HTML page, 1005 bytes, request-id absent.

Isolated with a direct `curl -X POST $ANTHROPIC_BASE_URL/v1/messages` (same headers `claude` would send) run inside the beam, with headers captured: **302 to `https://example.beams.sh:443/web/launch/anthropic.example.beams.sh?...`** — a login-page redirect, not an RBAC 403. The identical request from a **captain-owned** beam returns 200 with a real Anthropic response body. `tsh -i <bot> apps ls` and `tsh apps ls` (captain) list the same two apps (`anthropic`, `openai`), so this isn't the role failing to grant the app — RBAC-wise both identities see the app the same way. The LLM gateway's own authorization (separate from the Teleport role check `apps ls` exercises) doesn't accept the bot's beam the way it accepts the captain's. Whether that's keyed off something that only exists for beams created by a non-bot Teleport user, or a Beams-side allowlist that doesn't yet include bot identities, isn't answerable from the client side; this is exactly the "we don't know if Beams works for a bot at all" risk the spike was scoped to surface. Billing attribution (the second half of question 2) is moot: there's no successful call to bill.

**5. The bridge on the bot — ran, failed as a direct consequence of step 4.** Wrapper:

```sh
#!/bin/sh
exec tsh -i "$HOME/.bmus-spike84/tbot/identity" --proxy=example.beams.sh:443 "$@"
```

(the real `tsh` path on this machine; `/usr/local/bin/tsh` doesn't exist here). `BMUS_HOME=~/.bmus-spike84` avoided the process-lock collision with the already-running production bridge on `~/.bmus`. With that `PATH` and `BMUS_HOME`, `./bmus bridge run --repo bmus=$PWD` came up green, `crew add sorell` provisioned a beam and got combadge serving ("beam mint-chamber up, combadge serving; crew on deck"), and `order create` on the smoke order ran the mission, shipped the `bmus` repo into the beam, and then failed with the exact same Claude Code API error as step 4's manual repro. The bridge handled the failure cleanly: order state `failed`, crew back on deck, condition stayed `green`, no hang. No commit exists because the crew member never got far enough to make one.

**6. Renewal — pass (the mechanism), not reached (the order).** Restarted tbot with `--certificate-ttl=5m --renewal-interval=2m` (no bound-keypair state to reuse, since step 1 used the token join method; `--token`/`--join-method=token` had to be passed again on the CLI, but tbot's log shows it loaded the existing renewable identity from `--storage` and renewed from that, not from re-consuming the token). Left it running 4 minutes past the original 5-minute TTL with nobody touching the machine: `tsh status -i $IDENT` showed `Valid until` move from `14:52:59` to `14:56:58` with no interaction. A second order (`order2.yaml`) was created against the renewed identity with no re-login between the two orders, and failed with the identical inference error from step 4 — the renewal path itself works; the order-level pass criterion isn't reachable until question 2 is fixed.

**7. Audit and least privilege — partial.** `tsh -i <bot> ls` and `db ls` behave as section "Role used" above describes (scoped node, empty db). The captain-removes-bot's-beam and bot-removes-captain's-beam checks (both denied) are under step 3. **The audit-log actor check (question 6's other half) wasn't done**: this tsh/tctl build has no `tctl get events`/`tctl events` subcommand (confirmed against `tctl --help`'s full command list), so reading the audit log needs the web UI, which needs an interactive SSO login this session couldn't perform (a headless browser hit the login page; relaying to the user's own Chrome timed out with no window to adopt). Whoever has an authenticated browser against `https://example.beams.sh/web/cluster/example.beams.sh/audit/events` can check in under a minute whether `beam.destroyed` and the session events from steps 3–6 name `bot-bmus-bridge` as the actor; everything else in this spike points to yes (the beams list already showed `owner: bot-bmus-bridge` on every bot-created beam), but it wasn't directly observed.

### Cleanup

All spike-created beams were removed (`wise-current`, `full-trace`, `mint-chamber`, plus two captain-side throwaway beams used for the step-4 and step-7 comparisons). The spike's `bmus bridge run` was stopped with `SIGTERM` (graceful shutdown logged, no force-kill needed) and its `tbot` process killed. The `bmus-bridge` role and bot are left on the cluster for whoever picks up the follow-up issue; `tctl bots rm bmus-bridge` / `tctl rm role/bmus-bridge` removes them if that's not wanted. `~/.bmus-spike84` and `/tmp/bmus-spike-84` are untouched scratch state, safe to delete. The production bridge on `~/.bmus` (pid unchanged throughout) was never touched.
