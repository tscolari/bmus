
## Interacting with the project

Issues should be created in Github for any new work that is identified.
Issues should be updated everytime there's a new discovery or decision that affects them.

### Ticket hierarchy and labels

Each delivery phase in `./spec` (e.g. "Phase 1 — One crew member, end to end") gets exactly
one **Epic**: a GitHub issue with the `epic` label and a milestone named after the phase
(`Phase 1`, `Phase 2`, ...). Create the milestone when the epic is created, not before.

Concrete work items are plain issues, created as native GitHub sub-issues of their phase's
epic (`gh issue create --parent <epic-number>`), never as a checklist line inside the
epic's body — sub-issues show up in GitHub's own hierarchy UI and let `gh issue list`
filter by parent. GitHub issue types (`Task`/`Bug`/`Feature`) are configured at the
organization level and aren't available on `tscolari/bmus` since it's a personal-account
repo (confirmed: `gh issue create --type` rejects every type name, and GraphQL's
`repository.issueTypes` returns `null` for it) — don't try to set one, and don't block
on this changing; the `epic` label plus the parent/sub-issue relationship already carries
the distinction this would have encoded.

Real sequencing between tasks (one can't start until another lands) is recorded with native
issue relations (`gh issue create --blocked-by <number>` / `--blocking <number>`), not prose
like "depends on #12" in the body. GitHub's own Blocked badge is the single source of truth
for whether a task is pickupable — don't add a parallel `status:ready`/`status:blocked`
label, it can drift out of sync with the real relation.

Every issue carries:
- One `phase:N` label (0-5, matching the spec's delivery phases).
- One or more `area:*` labels naming the component(s) touched: `area:bridge`,
  `area:combadge`, `area:transporter`, `area:comms`, `area:store`, `area:cli`,
  `area:protocol`, `area:holodeck`, `area:console`, `area:docs`.

Issue body template (both epics and tasks):

    ## Context
    <1-2 sentences: what this is and why, with a link to the relevant file/section under ./spec>

    ## Change
    <concrete, ordered bullets — exact function/file/command names, not "update X">

    ## Acceptance
    <one or more concrete, checkable conditions — a command and its expected output, not "works correctly">

An agent picking up a task should find everything it needs to start in the issue body plus
the linked spec section — it should never need to ask what "handle the comms layer" means.

PRs should be open as the only way for code to reach `main`.

### Roadmap

`README.md` has a user-facing roadmap, one line per capability. Keep it current:
- a PR that ships a roadmap item ticks it (`- [x]`) in the same PR;
- a PR that adds or drops a user-facing capability adds or removes its line;
- a new phase epic or campaigns stage adds its items.

### Public by default

This repository is public. Never put private information in code, tests, docs, specs, examples, issues, PRs or commit messages:
- personal names, emails, usernames or home-directory paths;
- real cluster, host or beam addresses, and anything copied from a real run's output without redacting it;
- tokens, keys, passwords or any other credential, real or realistic-looking;
- employer-internal material: internal docs, roadmaps, unreleased product or feature names, dates, and private repositories.

Use placeholders: `example.com`, `example.beams.sh`, `captain@example.com`, `<token>`. Describe a feature bmus needs by what it has to do, and leave internal names out.

The `./spec` holds documents and decisions that were taken, additional to Github issues,
those files should include high level decisions and architectural decisions that were or
will be implemented.

Lets treat this directory as "incremental", new findings get added to dated files so
history can be rebuilt and decisions understood.

From time to time we might create a new "anchor" with all decisions up to that point.
