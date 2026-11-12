#!/usr/bin/env bash
# #145 hail-parking spike: runs spike.mjs inside a Teleport beam the captain
# owns, and copies its results back next to this script.
#
# Run it from your own machine, where `tsh` is logged in as the captain:
#
#   ./combadge/spike/hail-parking/spike.sh
#
# Env knobs:
#   BEAM=<id>       reuse this beam instead of creating one (never removed)
#   KEEP_BEAM=1     keep the beam this script created (default: removed on exit)
#   SPIKE_FORCE=1   run even when the gateway check says the beam can't reach the model
#   TSH=<path>      tsh binary (default: tsh on PATH)
#   SPIKE_*         every other SPIKE_* variable is passed to spike.mjs in the beam
#                   (SPIKE_SCENARIOS, SPIKE_STOP, SPIKE_MODEL, ...; see spike.mjs)
#
# Nothing here prints environment variables or keys.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TSH="${TSH:-tsh}"
ts="$(date -u +%Y%m%dT%H%M%SZ)"

say() { printf '==> %s\n' "$*"; }
warn() { printf '!!! %s\n' "$*" >&2; }
die() {
  printf 'spike.sh: %s\n' "$*" >&2
  exit 1
}

# sq STRING: STRING single-quoted for a POSIX shell. The quotes live in
# variables: bash before 4.3 (macOS's 3.2) doesn't remove quotes from a
# double-quoted ${x//pat/rep}'s replacement.
sq() {
  local q="'" r="'\\''"
  printf "'%s'" "${1//$q/$r}"
}

# bsh SCRIPT: runs SCRIPT with sh in the beam. tsh joins its arguments with
# spaces into one command line the beam's shell parses again (see
# internal/transporter/tsh.go Exec), so the script goes over single-quoted.
bsh() { "$TSH" beams exec "$BEAM" -- sh -c "$(sq "$1")"; }

# json_field FIELD: FIELD (a string) of the first JSON object on stdin; jq, else
# sed. Prints nothing (and succeeds) when stdin isn't JSON.
json_field() {
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg f "$1" 'if type == "array" then .[0] else . end | .[$f] // empty' 2>/dev/null || true
  else
    sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -n 1
  fi
}

# ---- local preflight ----

for c in "$TSH" node npm; do
  command -v "$c" >/dev/null 2>&1 || die "$c not found on PATH"
done
for f in spike.mjs package.json package-lock.json; do
  [[ -f "$here/$f" ]] || die "missing $here/$f"
done
[[ "$here" != *:* ]] || die "tsh beams scp reads a local path with ':' as remote; move the checkout ($here)"
say "local: $("$TSH" version 2>/dev/null | head -n 1), node $(node --version), npm $(npm --version)"

# The captain is `tsh status --format=json`'s .active.username. An expired
# login (or one with under 5 minutes left) stops here: tsh would otherwise try
# to log in again halfway through, prompting or opening a browser.
status_rc=0
status="$("$TSH" status --format=json 2>/dev/null)" || status_rc=$?
# shellcheck disable=SC2016 # a JavaScript template literal
login="$(printf '%s' "$status" | node -e '
  let s = "";
  process.stdin.on("data", (d) => (s += d)).on("end", () => {
    let j;
    try { j = JSON.parse(s.slice(s.indexOf("{"))); } catch {}
    const a = j && j.active;
    if (!a || !a.username) return console.log("none");
    const until = a.valid_until ? Date.parse(a.valid_until) : NaN;
    const expired = Number.isFinite(until) && until > 0 && until - Date.now() < 5 * 60e3;
    console.log(`${expired ? "expired" : "ok"} ${a.username} ${a.valid_until || "-"}`);
  });
')"
read -r login_state captain valid_until <<<"$login"
case "$login_state" in
  ok) say "tsh logged in as $captain (valid until $valid_until)" ;;
  expired) die "tsh login for $captain expires at $valid_until (or already has); run 'tsh login' first" ;;
  *) die "tsh doesn't look logged in (no active profile in 'tsh status', exit $status_rc); run 'tsh login' first" ;;
esac

# ---- beam ----

created=0
tmp=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
  local rc=$?
  [[ -z "$tmp" ]] || rm -rf "$tmp"
  if [[ "$created" == 1 ]]; then
    if [[ "${KEEP_BEAM:-}" == 1 ]]; then
      say "keeping beam $BEAM (KEEP_BEAM=1); remove it with: $TSH beams rm $BEAM"
    else
      say "removing beam $BEAM"
      "$TSH" beams rm "$BEAM" >/dev/null 2>&1 || warn "could not remove beam $BEAM; remove it with: $TSH beams rm $BEAM"
    fi
  elif [[ -n "${BEAM:-}" && -n "${rdir:-}" ]]; then
    say "beam $BEAM was reused and is left as it is; the spike's files are in $rdir"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

owner=""
if [[ -n "${BEAM:-}" ]]; then
  say "reusing beam $BEAM"
  if command -v jq >/dev/null 2>&1; then
    owner="$("$TSH" beams ls --format=json 2>/dev/null | jq -r --arg id "$BEAM" '.[] | select(.id == $id) | .owner // empty' || true)"
  fi
else
  say "creating a beam (tsh beams add --no-console --format=json)"
  out="$("$TSH" beams add --no-console --format=json)"
  BEAM="$(printf '%s' "$out" | json_field id)"
  if [[ -z "$BEAM" ]]; then
    printf '%s\n' "tsh beams add printed:" "$out" >&2
    die "could not read the beam id from that; a beam may have been created: check '$TSH beams ls' and remove it with '$TSH beams rm <id>'"
  fi
  created=1
  owner="$(printf '%s' "$out" | json_field owner)"
  say "created beam $BEAM"
fi
if [[ -z "$owner" ]]; then
  warn "could not tell who owns beam $BEAM; inference only works from a beam the captain created (spec/spike-tbot-bridge-identity-2026-10-18.md step 4)"
elif [[ "$owner" != "$captain" ]]; then
  warn "beam $BEAM is owned by '$owner', not '$captain': inference only works from a beam the captain created (spec/spike-tbot-bridge-identity-2026-10-18.md step 4)"
else
  say "beam owner: $owner"
fi

# ---- beam preflight ----

# shellcheck disable=SC2016 # expanded by the beam's shell
info="$(bsh 'printf "node=%s\n" "$(node --version 2>/dev/null)"; printf "npm=%s\n" "$(npm --version 2>/dev/null)"; printf "arch=%s\n" "$(uname -m)"; printf "claude=%s\n" "$(claude --version 2>/dev/null | head -n 1)"' | tr -d '\r')"
beam_node="$(printf '%s\n' "$info" | sed -n 's/^node=//p')"
beam_npm="$(printf '%s\n' "$info" | sed -n 's/^npm=//p')"
beam_arch="$(printf '%s\n' "$info" | sed -n 's/^arch=//p')"
beam_claude="$(printf '%s\n' "$info" | sed -n 's/^claude=//p')"
say "beam: node ${beam_node:-MISSING}, npm ${beam_npm:-missing}, $beam_arch, claude ${beam_claude:-not on PATH, so the SDK uses its bundled binary}"
[[ -n "$beam_node" ]] || die "node is not on the beam's PATH"
case "$beam_arch" in
  x86_64 | amd64) cpu=x64 ;;
  aarch64 | arm64) cpu=arm64 ;;
  *) die "unknown beam architecture '$beam_arch'" ;;
esac

# The LLM gateway, without spending a model call: an empty body to
# $ANTHROPIC_BASE_URL/v1/messages answers 400 when the key is accepted, and a
# 302 to the login page from a bot-owned beam (spec/spike-tbot-bridge-identity-2026-10-18.md
# step 4). Keys go to curl on stdin, never on its command line or ours.
# shellcheck disable=SC2016 # expanded by the beam's shell
gateway="$(bsh '
  [ -n "${ANTHROPIC_BASE_URL:-}" ] || { echo unset; exit 0; }
  command -v curl >/dev/null 2>&1 || { echo nocurl; exit 0; }
  {
    printf "anthropic-version: 2023-06-01\ncontent-type: application/json\n"
    [ -n "${ANTHROPIC_API_KEY:-}" ] && printf "x-api-key: %s\n" "$ANTHROPIC_API_KEY"
    [ -n "${ANTHROPIC_AUTH_TOKEN:-}" ] && printf "authorization: Bearer %s\n" "$ANTHROPIC_AUTH_TOKEN"
    true
  } | curl -s -o /dev/null -w "%{http_code}" --max-time 20 -X POST -H @- --data "{}" "${ANTHROPIC_BASE_URL%/}/v1/messages" || echo curl-failed
' | tr -d '\r')"
case "$gateway" in
  200 | 400) say "gateway check: HTTP $gateway, the key is accepted" ;;
  302 | 301 | 307) gw_bad="HTTP $gateway: the gateway redirects to a login page, as it does for a bot-owned beam" ;;
  401 | 403) gw_bad="HTTP $gateway: the gateway rejects the beam's key" ;;
  unset) gw_bad="ANTHROPIC_BASE_URL is not set in the beam" ;;
  nocurl) warn "no curl in the beam; skipping the gateway check" ;;
  *) warn "gateway check: unexpected answer '$gateway'; carrying on" ;;
esac
if [[ -n "${gw_bad:-}" ]]; then
  [[ "${SPIKE_FORCE:-}" == 1 ]] || die "gateway check: $gw_bad (SPIKE_FORCE=1 runs anyway)"
  warn "gateway check: $gw_bad; running anyway (SPIKE_FORCE=1)"
fi

# ---- copy ----

rdir="$(bsh 'mktemp -d /tmp/hail-parking-spike.XXXXXX' | tr -d '\r')"
[[ "$rdir" == /tmp/hail-parking-spike.* ]] || die "could not make a remote dir (got '$rdir')"
say "remote dir $BEAM:$rdir"
for f in spike.mjs package.json package-lock.json; do
  "$TSH" beams scp "$here/$f" "$BEAM:$rdir/$f" >/dev/null
done

# ---- install ----

# No retries and a short fetch timeout, so a beam without a registry falls back quickly.
if bsh "cd $rdir && npm ci --no-audit --no-fund --loglevel=error --fetch-retries=0 --fetch-timeout=20000"; then
  install_path="beam: npm ci inside the beam"
else
  warn "npm ci failed inside the beam (no registry access?); installing locally for linux/$cpu and copying node_modules over"
  tmp="$(mktemp -d)"
  cp "$here/package.json" "$here/package-lock.json" "$tmp/"
  (cd "$tmp" && npm ci --os=linux --cpu="$cpu" --no-audit --no-fund --loglevel=error)
  tarflags=()
  [[ "$(uname -s)" != Darwin ]] || tarflags=(--no-xattrs) # no AppleDouble or xattr noise on linux
  COPYFILE_DISABLE=1 tar "${tarflags[@]+"${tarflags[@]}"}" -czf "$tmp/node_modules.tgz" -C "$tmp" node_modules
  "$TSH" beams scp "$tmp/node_modules.tgz" "$BEAM:$rdir/node_modules.tgz" >/dev/null
  bsh "cd $rdir && tar -xzf node_modules.tgz && rm -f node_modules.tgz"
  install_path="local: npm ci --os=linux --cpu=$cpu on this machine, node_modules shipped as a tarball"
fi
say "install path: $install_path"
sdk="$(bsh "cd $rdir && node -p \"require('./node_modules/@anthropic-ai/claude-agent-sdk/package.json').version\"" | tr -d '\r')"
say "agent sdk in the beam: $sdk"

# ---- run ----

# Every SPIKE_* knob but the ones meant for this script goes to spike.mjs.
knobs=()
while IFS='=' read -r name _; do
  case "$name" in
    SPIKE_FORCE | SPIKE_DRY) ;;
    SPIKE_*) knobs+=("$name=${!name}") ;;
  esac
done < <(env | grep '^SPIKE_[A-Z_]*=' || true)
envline=""
for k in "${knobs[@]+"${knobs[@]}"}"; do envline+=" $(sq "$k")"; done

say "dry run in the beam (loads the SDK, no model calls)"
bsh "cd $rdir && SPIKE_DRY=1 node spike.mjs >/dev/null" || die "spike.mjs failed its dry run in the beam"

stdout_log="$here/spike-stdout-$BEAM-$ts.log"
say "running spike.mjs in the beam (a few model calls; each step times out on its own)"
warn "tsh beams exec reruns a command (up to 10 times) when the connection drops: a flaky link can run the spike twice. Each run writes its own results file, and all of them are copied back."
{
  echo "install path: $install_path"
  echo "beam $BEAM owner ${owner:-unknown}, captain $captain, gateway check ${gateway:-?}"
} >"$stdout_log"
set +e
bsh "cd $rdir && env$envline node spike.mjs" 2>&1 | tee -a "$stdout_log"
rc=${PIPESTATUS[0]}
set -e

# ---- results ----

# SPIKE_RESULTS when set (relative to the remote dir), else every
# spike-results-<run>.jsonl spike.mjs wrote there (more than one: tsh retried).
if [[ -n "${SPIKE_RESULTS:-}" ]]; then
  case "$SPIKE_RESULTS" in
    /*) remote_results=("$SPIKE_RESULTS") ;;
    *) remote_results=("$rdir/$SPIKE_RESULTS") ;;
  esac
else
  remote_results=()
  while IFS= read -r f; do
    [[ -n "$f" ]] && remote_results+=("$f")
  done < <(bsh "ls -1 $rdir/spike-results-*.jsonl 2>/dev/null || true" | tr -d '\r')
fi
(( ${#remote_results[@]} <= 1 )) || warn "found ${#remote_results[@]} results files: tsh ran the spike more than once; send them all"
n=0
for f in "${remote_results[@]+"${remote_results[@]}"}"; do
  n=$((n + 1))
  suffix=""
  (( n == 1 )) || suffix="-$n"
  results="$here/spike-results-$BEAM-$ts$suffix.jsonl"
  if "$TSH" beams scp "$BEAM:$f" "$results" >/dev/null; then
    say "results: $results"
  else
    warn "could not copy $f back from $BEAM"
  fi
done
(( n > 0 )) || warn "no results file found in $BEAM:$rdir"
say "stdout:  $stdout_log"
say "install path: $install_path"
[[ "$rc" == 0 ]] || warn "spike.mjs exited with $rc"
exit "$rc"
