#!/usr/bin/env bash
#
# tests/gate-declaration-self-test.sh — the proof that gate.yml is a gate and
# not a note.
#
#   tests/gate-declaration-self-test.sh
#
# WHAT THIS IS FOR
#
# A declaration that has only ever been green is a comment with a YAML
# extension. This script takes a copy of this repository's gate.yml, breaks
# exactly one thing in it at a time, and asserts core's checker goes RED and
# NAMES the finding it expects. Naming matters: "something went red" is a weak
# claim when twenty checks can go red, and the check written for one specific
# defect can be dead code forever while everything stays green.
#
# The shape is muse's `tests/gate_self_test.sh` and darkroom's
# `bin/gate-self-test`, for their reasons: a control on the unmodified
# declaration FIRST, a fresh throwaway copy per breakage (one must never mask
# the next), and a non-zero exit if any breakage stayed green.
#
# WHAT IS COPIED, AND WHY IT IS NOT THE WHOLE TREE
#
# `gate_check.py` reads four things — gate.yml, mise.toml, the entrypoint, and
# the CI workflow — and it runs `gate.command` with the copy as its working
# directory. A copy of those four paths IS a repository as far as the checker is
# concerned. Nothing in the committed tree is a deliberately broken repository:
# the breakages are diffs applied to throwaway copies, so a reviewer reads what
# is being broken rather than reconstructing it.
#
# THE STAND-IN GATE, STATED PLAINLY BECAUSE IT MATTERS
#
# Most of the `--prove` breakages below need the checker to RUN a gate, and each
# of those runs is a full `go test ./...` — about fifteen seconds. Twelve of them
# would be a self-test nobody runs, and this file's own rule is no sleeps and no
# raised retries. So they run a stand-in that prints the two `caf:` lines the
# real gate prints, captured verbatim from a real run.
#
# The stand-in is NOT evidence that the gate works. That is the first control,
# and it runs the real `bin/prime`. What the stand-in buys is that the breakages
# which are about the SHAPE of a proof — a pattern with no capture group, a
# floor above the count — cost a second each instead of a suite run. Two
# breakages do use the real gate, and they are marked: `floor`, which is a claim
# about this repository's actual test count, and the `--live` case, which is a
# claim about this repository's gate and cannot be staged with a stand-in.
#
# THE TWO THAT ARE ONLY THIS REPOSITORY'S
#
# The last two breakages are about caf specifically and would be meaningless in
# another repository. One removes the live-tier accounting line and asserts
# `gate.proof-missing` — the "a tier that must not say it skipped" case, which
# is the whole reason `live-tier` has no floor. The other runs `bin/prime
# --live` on a PATH with no container runtime on it, hermetically and with no
# sleep, and asserts the gate refuses: the live tier is a demonstration, and a
# demonstration that did not happen is not a pass.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# --------------------------------------------------------------------------
# Locate core's checker. There is no copy in this repository on purpose:
# `gate_check.py` travels with core's `harness/cafaye_contract.py`, and a
# vendored copy of one without the other would be a second YAML dialect.
#
# A checker that could not be found exits 2 and never 0. A missing checkout is
# not a clean bill of health, it is an unknown, and this script reporting "all
# breakages caught" because it proved nothing is the exact failure mode this
# packet exists to remove.
# --------------------------------------------------------------------------
CHECKER="${CAFAYE_GATE_CHECK:-}"
if [ -z "$CHECKER" ]; then
  for candidate in "$ROOT/../core/harness/gate_check.py" "$ROOT/../../core/harness/gate_check.py"; do
    if [ -r "$candidate" ]; then CHECKER="$candidate"; break; fi
  done
fi

if [ -z "$CHECKER" ] || [ ! -r "$CHECKER" ]; then
  echo "gate-declaration-self-test: core's harness/gate_check.py was not found." >&2
  echo "  This script proves that gate.yml can fail, and that is impossible" >&2
  echo "  without the checker. Point CAFAYE_GATE_CHECK at the file, or check" >&2
  echo "  out cafaye/core next to this repository. Exiting 2: the check could" >&2
  echo "  not happen, which is not the same as having passed." >&2
  exit 2
fi

# core's checker reads mise.toml with tomllib, which is stdlib from 3.11 (D31).
PY="${CAFAYE_GATE_PYTHON:-}"
if [ -z "$PY" ]; then
  for candidate in python3 python3.14 python3.13 python3.12 python3.11 python; do
    command -v "$candidate" >/dev/null 2>&1 || continue
    PY="$candidate"
    break
  done
fi
if [ -z "$PY" ] || ! "$PY" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)'; then
  echo "gate-declaration-self-test: gate_check.py needs python >= 3.11 (tomllib); none found." >&2
  echo "  Set CAFAYE_GATE_PYTHON. Exiting 2, for the same reason as above." >&2
  exit 2
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/caf-gate-self-test.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

# Four counters, not one, because a single total cannot be read: a run saying
# "15 passed" could be fifteen reds or ten reds and five controls, and those are
# different claims. A skipped check is not a passing check, so `skipped` is
# reported as its own number and never folded into a pass count.
failures=0
controls=0      # cases that must come back GREEN
breakages=0     # cases that must go RED and name their finding
warn_cases=0    # cases that must warn AND still exit 0
skipped=0       # cases not run; reported, never counted as a pass

# --------------------------------------------------------------------------
# helpers
# --------------------------------------------------------------------------

# fresh_copy <name> — a repository the checker cannot tell from this one.
#
# `.github` is here and not an afterthought: the workflow is the artifact
# `gate.ci-disagrees` reads by path, so a copy without it would fail on a
# missing file rather than on the defect under test.
fresh_copy() {
  local name="$1"
  local dst="$WORK/$name"
  rm -rf "$dst"
  mkdir -p "$dst/.github/workflows" "$dst/bin"
  cp "$ROOT/gate.yml" "$dst/gate.yml"
  cp "$ROOT/mise.toml" "$dst/mise.toml"
  cp "$ROOT/bin/prime" "$dst/bin/prime"
  cp "$ROOT/.github/workflows/ci.yml" "$dst/.github/workflows/ci.yml"
  chmod +x "$dst/bin/prime"
  printf '%s' "$dst"
}

# edit <file> <old> <new> — a textual breakage that FAILS LOUDLY if this
# repository has moved past it. A self-test that silently stops breaking
# anything is worse than no self-test: an unmatched edit means the recipe is
# stale, and a stale recipe that "passes" is the whole class of defect here.
edit() {
  "$PY" - "$1" "$2" "$3" <<'PY'
import sys

path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
body = open(path, encoding="utf-8").read()
if old not in body:
    sys.exit(f"gate-declaration-self-test: breakage no longer applies to {path}: {old!r} not found")
open(path, "w", encoding="utf-8").write(body.replace(old, new, 1))
PY
  if [ $? -ne 0 ]; then
    echo "FAIL gate-declaration-self-test: a breakage recipe no longer applies. The proof it was" >&2
    echo "       written to provide does not exist any more." >&2
    failures=$((failures + 1))
  fi
}

# write <file> — a breakage that replaces a whole file.
write() { cat > "$1"; }

# install_standin <dir> [top-level passes] — the gate the proving breakages run.
# The count is a parameter so one breakage can print a suite that shrank.
#
# The two lines below are copied character for character out of a real
# `bin/prime` run on this tree, because a stand-in that prints a paraphrase is
# a stand-in that proves the paraphrase matches.
#
# WITH NO COUNT, THE COUNT IS READ OUT OF `gate.yml` AND NOT WRITTEN DOWN HERE.
# It was `432` in this file until the floor moved to 435, and the stand-in
# control went red naming `gate.floor` — which is the checker working and this
# script carrying the defect billing's did. The number was in two files and the
# merge made one of them wrong; the fix is not to write down 435 instead, which
# would break the same way on the next packet that adds a test. It is to have
# one place that says it. Read the same way the ratchet in `internal/ci` reads
# it, from the `suite` proof's own floor, so the stand-in is green by
# construction against whatever the declaration currently claims.
suite_floor() {
  sed -n '/^    - id: suite$/,/^    - id: /{s/^      minimum: \([0-9][0-9]*\)$/\1/p;}' \
    "$ROOT/gate.yml" | head -1
}

# packages_floor is the `packages` proof's own minimum, read for the reason
# suite_floor is: the stand-in used to print a literal `10`, which went stale the
# same way the suite floor did when caf-21 added `internal/deploy`, and the
# stand-in control went red naming gate.floor.
packages_floor() {
  sed -n '/^    - id: packages$/,/^    - id: /{s/^      minimum: \([0-9][0-9]*\)$/\1/p;}' \
    "$ROOT/gate.yml" | head -1
}

# subtests_floor is the `subtests` proof's minimum, and the stand-in's hardcoded
# 537 is stale for the same reason. Read here so the stand-in is green by
# construction against whatever the declaration currently claims.
subtests_floor() {
  sed -n '/^    - id: subtests$/,/^    - id: /{s/^      minimum: \([0-9][0-9]*\)$/\1/p;}' \
    "$ROOT/gate.yml" | head -1
}

install_standin() {
  local dir="$1" passes="${2:-}"
  if [ -z "$passes" ]; then
    passes="$(suite_floor)"
    if [ -z "$passes" ]; then
      echo "gate-declaration-self-test: no suite floor found in $ROOT/gate.yml." >&2
      echo "  If the declaration's shape changed, fix suite_floor — do not put the" >&2
      echo "  number back in this file, which is how it went stale the first time." >&2
      failures=$((failures + 1))
      return 1
    fi
  fi
  local packages subtests
  packages="$(packages_floor)"
  subtests="$(subtests_floor)"
  if [ -z "$packages" ] || [ -z "$subtests" ]; then
    echo "gate-declaration-self-test: no packages or subtests floor found in $ROOT/gate.yml." >&2
    echo "  If the proofs' shape changed, fix packages_floor and subtests_floor — do not" >&2
    echo "  put the numbers back in this file, which is how they went stale." >&2
    failures=$((failures + 1))
    return 1
  fi
  # The live tier's SIZE is read out of gate.yml for the same reason the suite
  # floor is, and it went stale the same way: the literal `2` below survived
  # caf-21, which added two live deploy tests, and the `live-tier` proof stopped
  # matching — so the stand-in control went red naming a proof that gate.yml
  # still declares. `internal/ci`'s TestTheGateAccountsForEveryLiveTest keeps
  # bin/prime and the tree in agreement; this keeps the stand-in and gate.yml in
  # agreement, which is the other half and was not checked at all.
  local tier
  tier="$(live_tier_size)"
  if [ -z "$tier" ]; then
    echo "gate-declaration-self-test: no live tier size found in $ROOT/gate.yml." >&2
    echo "  If the proof's shape changed, fix live_tier_size — do not put the" >&2
    echo "  number back in this file." >&2
    failures=$((failures + 1))
    return 1
  fi
  write "$dir/bin/prime" <<STUB
#!/usr/bin/env bash
# A stand-in for bin/prime, written by tests/gate-declaration-self-test.sh.
# Prints the two lines the real gate prints and nothing else, so a proving
# breakage costs a second instead of a full suite run. It is NOT evidence that
# the real gate works: the first control runs the real bin/prime.
set -euo pipefail
echo "ok  \tgithub.com/cafaye/caf/internal/example\t0.001s"
echo 'caf: ${packages} packages, ${passes} top-level passes, ${subtests} subtest passes, 0 failures, 3 skips'
echo 'caf: live tier: 0 of ${tier} executed; not enabled, so the live tests skipped rather than ran. Set CAF_LIVE_DOCKER=1, CAF_LIVE_RYUK=1 and CAF_LIVE_KAMAL=1, or run bin/prime --live'
STUB
  chmod +x "$dir/bin/prime"
}

# live_tier_size is the number in gate.yml's `live-tier` proof pattern, read the
# same way suite_floor reads the suite floor. It is written as "of N executed" and
# the N is the tier's size, which is also the `2` bin/prime writes into its own
# line — so the two have to be one number and neither of them may be written down
# here.
live_tier_size() {
  sed -n '/^    - id: live-tier$/,$ {s/^      match: .*of \([0-9][0-9]*\) executed.*$/\1/p;}' \
    "$ROOT/gate.yml" | head -1
}

# check <dir> [args...] — run the checker, capture output and exit code.
#
# The exit code is read from the command substitution's own status, never from
# `$?` of a pipeline: this fleet's one recorded false green was exactly that
# mistake, under zsh, where `$?` was `tail`'s.
out=""
code=0
check() {
  local dir="$1"
  shift
  out="$("$PY" "$CHECKER" --log-dir "$dir/.log" "$@" "$dir" 2>&1)" || code=$?
}

# expect_red <label> <dir> <finding-id> [args...]
# The exit code must be 1 AND the report must NAME the finding. Both halves.
expect_red() {
  local label="$1" dir="$2" expect="$3"
  shift 3
  code=0
  check "$dir" "$@"
  if [ "$code" -ne 1 ]; then
    printf 'FAIL gate-declaration-self-test: %s — expected exit 1, got %s\n%s\n' "$label" "$code" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  if ! printf '%s' "$out" | grep -qF "$expect"; then
    printf 'FAIL gate-declaration-self-test: %s — went red as something else and never said %s\n%s\n' \
      "$label" "$expect" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  breakages=$((breakages + 1))
  printf 'PASS gate-declaration-self-test: breakage %s: %s — caught by `%s`\n' \
    "$breakages" "$label" "$expect"
}

# expect_green <label> <dir> [args...]
expect_green() {
  local label="$1" dir="$2"
  shift 2
  code=0
  check "$dir" "$@"
  if [ "$code" -ne 0 ]; then
    printf 'FAIL gate-declaration-self-test: %s — expected exit 0, got %s\n%s\n' "$label" "$code" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  controls=$((controls + 1))
  printf 'PASS gate-declaration-self-test: control %s: %s\n' "$controls" "$label"
}

# expect_green_proving <label> <dir> — expect_green with `--prove` spelled out,
# so every proving case reads the same way and a reader can count them.
expect_green_proving() {
  local label="$1" dir="$2"
  shift 2
  expect_green "$label" "$dir" --prove "$@"
}

# expect_warn <label> <dir> <finding-id> [args...]
# The finding must be printed AND the exit code must STILL be 0. The exit code
# is the half that matters: `gate.requirement-unproven` means "this machine
# could not answer that", and a checker that laundered it into a failure would
# be red on a laptop and green on CI.
expect_warn() {
  local label="$1" dir="$2" expect="$3"
  shift 3
  code=0
  check "$dir" "$@"
  if [ "$code" -ne 0 ]; then
    printf 'FAIL gate-declaration-self-test: %s — a warning moved the exit code to %s\n%s\n' \
      "$label" "$code" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  if ! printf '%s' "$out" | grep -qF "$expect"; then
    printf 'FAIL gate-declaration-self-test: %s — exited 0 without even printing %s\n%s\n' \
      "$label" "$expect" "$out" >&2
    failures=$((failures + 1))
    return
  fi
  warn_cases=$((warn_cases + 1))
  printf 'PASS gate-declaration-self-test: warning %s: %s — said `%s` and still exited 0\n' \
    "$warn_cases" "$label" "$expect"
}

# --------------------------------------------------------------------------
# THE CONTROLS. Without them, every red below proves nothing: a checker that
# refused everything would satisfy every expectation in this file.
# --------------------------------------------------------------------------

# Control 1 — static, on this repository, unmodified.
expect_green 'the static control — a declaration that is true of this repository' "$ROOT"

# Control 2 — the REAL gate, proved. This is the only evidence that the
# patterns in gate.yml match what bin/prime actually prints, and it is why the
# stand-in below is allowed to stand in for the rest.
expect_green_proving 'the proving control — the real bin/prime ran and every declared proof appeared at or above its floor' \
  "$ROOT"

# Control 3 — the stand-in, proved. It earns the right to stand in for the
# proving breakages below by being shown to satisfy the same declaration the
# real gate satisfies.
standin_control="$(fresh_copy standin-control)"
install_standin "$standin_control"
expect_green_proving 'the stand-in control — the stub gate satisfies the same declaration the real one does' \
  "$standin_control"

# --------------------------------------------------------------------------
# THE BREAKAGES: the declaration disagreeing with the tree
# --------------------------------------------------------------------------

b="$(fresh_copy declaration-missing)"
rm -f "$b/gate.yml"
expect_red 'a repository that declares no gate at all' "$b" 'gate.declaration-missing'

b="$(fresh_copy command-missing)"
edit "$b/gate.yml" 'command: [bin/prime]' 'command: [bin/absent]'
expect_red 'a gate command naming a file this repository does not have' "$b" 'gate.command-missing'

b="$(fresh_copy entrypoint-missing)"
edit "$b/gate.yml" 'entrypoint: bin/prime' 'entrypoint: bin/absent'
expect_red 'a gate entrypoint this repository does not have, while the command still does' \
  "$b" 'gate.entrypoint-missing'

b="$(fresh_copy entrypoint-not-executable)"
chmod -x "$b/bin/prime"
expect_red 'a gate nobody is allowed to execute' "$b" 'gate.entrypoint-not-executable'

b="$(fresh_copy task-missing)"
edit "$b/gate.yml" 'miseTask: prime' 'miseTask: verify'
expect_red 'a mise task that is not in this repository'"'"'s mise config' "$b" 'gate.task-missing'

# The brief names this one, and it is the check that makes `miseTask` worth
# declaring: the task and the entrypoint are two readings of one gate, and this
# is what catches them disagreeing.
b="$(fresh_copy task-unresolvable)"
edit "$b/mise.toml" 'run = "./bin/prime"' 'run = "./bin/some-other-file"'
expect_red 'a mise task that resolves to a different file than the entrypoint names' \
  "$b" 'gate.task-unresolvable'

b="$(fresh_copy task-config-missing)"
rm -f "$b/mise.toml"
expect_red 'a mise task named in a repository that has no mise config' \
  "$b" 'gate.task-config-missing'

b="$(fresh_copy ci-missing)"
edit "$b/gate.yml" 'workflow: .github/workflows/ci.yml' 'workflow: .github/workflows/nope.yml'
expect_red 'a declaration naming a CI workflow that is not in this repository' "$b" 'gate.ci-missing'

# Also named in the brief. The mutation is not a deleted file — it is a
# workflow that exists, runs, and simply never runs the gate, which is the
# drift `gate.ci-disagrees` exists to catch. It replaces the plain one-line
# `run: ./bin/prime`, deliberately, because as of core 63fd319 that spelling is
# visible to the checker and this breakage is what proves it.
b="$(fresh_copy ci-disagrees)"
edit "$b/.github/workflows/ci.yml" './bin/prime' 'echo "the tests passed"'
expect_red 'a CI workflow that exists, runs, and never invokes the gate' "$b" 'gate.ci-disagrees'

b="$(fresh_copy requirement-path-missing)"
edit "$b/gate.yml" 'command: [go, mod, download]' 'command: [bin/not-a-setup-script]'
expect_red 'a requirement satisfied by a file this repository does not have' \
  "$b" 'gate.requirement-path-missing'

# The exact command that produced this fleet's one recorded false green:
# `… | tail -45; echo "PRIME EXIT=$?"`, where `$?` is tail's and tail is always
# zero. The schema refuses it, and `examples/invalid/gate.shell-string.yml` in
# core is the same refusal. A declaration that could hold it would be able to
# describe a gate whose exit code is not the gate's.
b="$(fresh_copy shell-string)"
edit "$b/gate.yml" 'command: [bin/prime]' "command: ['bin/prime 2>&1 | tail -45']"
expect_red 'a gate command written as a shell string with a pipeline in it' "$b" 'gate.schema'

# A proof whose pattern will not compile. Caught at the SHAPE layer, which is
# the better answer: the alternative is discovering it after spending the
# gate's whole timeoutSeconds.
b="$(fresh_copy proof-uncompilable)"
edit "$b/gate.yml" "match: '^caf: ([0-9]+) packages, [0-9]+ top-level passes" \
  "match: '^caf: ([0-9]+ packages, [0-9]+ top-level passes"
expect_red 'a proof pattern that does not compile, which would otherwise read as "no proof required"' \
  "$b" 'gate.schema'

# `selfContained: true` with three requirements on it. The schema's conditional
# is the identity defect told the other way round: a gate that calls itself
# self-contained while naming what it needs.
b="$(fresh_copy self-contained-contradiction)"
edit "$b/gate.yml" '  selfContained: false' '  selfContained: true'
expect_red 'a gate that calls itself self-contained while naming three things it needs' \
  "$b" 'gate.schema'

# There is deliberately NO static counterpart to the "no capture group" breakage
# below. It was written, run against the static phase, and it stayed green — a
# proof with a floor and no group to read it from is a fact the static phase
# does not check at all. `gate.proof-invalid` is a proving-phase finding, and a
# declaration that promises a floor it cannot read is green until somebody runs
# the gate. Recording it here because "it stayed green" is exactly the kind of
# result a reader needs told rather than left to rediscover.

# --------------------------------------------------------------------------
# THE WARNINGS, AND THE PROMISE THEY MAKE
# --------------------------------------------------------------------------
#
# These assert the exit code stays 0 while the finding is printed. The
# tri-state contract made mechanical: a warning is a claim this machine could
# not settle, and a checker that laundered it into a failure would be red on a
# laptop and green on CI — the same defect in a new place.

# Every requirement in this declaration is satisfied by a bare name on PATH,
# which the checker deliberately does not run, so every run of this repository
# prints `gate.requirement-unproven` three times and exits 0. Asserting that
# here makes "three warnings on every run" a tested property rather than
# something a reader has to notice in a log.
b="$(fresh_copy requirement-unproven)"
expect_warn 'a requirement satisfied by a bare name on PATH, which this checker did not run' \
  "$b" 'gate.requirement-unproven'

b="$(fresh_copy task-unreadable)"
edit "$b/mise.toml" 'run = "./bin/prime"' 'run = "./bin/prime | tee /dev/null"'
expect_warn 'a mise task whose run string is a pipeline, so only a shell could say what it runs' \
  "$b" 'gate.task-unreadable'

b="$(fresh_copy ci-undeclared)"
"$PY" - "$b/gate.yml" <<'PY'
import sys

path = sys.argv[1]
lines = open(path, encoding="utf-8").read().splitlines(keepends=True)
kept, dropping, seen = [], False, False
for line in lines:
    if not dropping and line.startswith("ci:"):
        dropping, seen = True, True
        continue
    if dropping and line and not line[0].isspace():
        dropping = False
    if not dropping:
        kept.append(line)
if not seen:
    sys.exit("gate-declaration-self-test: no top-level ci: block in gate.yml")
open(path, "w", encoding="utf-8").write("".join(kept))
PY
expect_warn 'a declaration that says nothing about CI, so nothing checks that CI runs this gate' \
  "$b" 'gate.ci-undeclared'

b="$(fresh_copy task-undeclared)"
"$PY" - "$b/gate.yml" <<'PY'
import re
import sys

path = sys.argv[1]
body = open(path, encoding="utf-8").read()
new, n = re.subn(r"\n  miseTask: prime\n", "\n", body, count=1)
if n != 1:
    sys.exit("gate-declaration-self-test: expected exactly one miseTask line to remove")
open(path, "w", encoding="utf-8").write(new)
PY
expect_warn 'a repository with mise tasks and a declaration that names none of them' \
  "$b" 'gate.task-undeclared'

b="$(fresh_copy command-unknown)"
edit "$b/gate.yml" 'command: [bin/prime]' 'command: [caf-gate-not-on-any-path]'
expect_warn 'a gate command that is a bare name this machine does not have' \
  "$b" 'gate.command-unknown'

# --------------------------------------------------------------------------
# THE BREAKAGES THAT NEED THE GATE TO HAVE RUN
# --------------------------------------------------------------------------
#
# Every finding from here down is a PROVING-phase finding. That is worth saying
# out loud because it was a real bug in darkroom's first version of this script:
# `gate.proof-missing`, `gate.nonzero`, `gate.timeout`, `gate.floor` and
# `gate.proof-invalid` do not exist in the static phase at all, so a case written
# against one of them and run WITHOUT `--prove` cannot fail, and reports a clean
# static run instead. That is how five cases passed for one whole run while
# testing nothing. Every case below passes `--prove` explicitly.

# The one that is not string matching. Every string in the declaration about
# this copy is true — the command exists, it is executable, the mise task
# resolves to it, CI calls it — and the repository is ungated. Nothing about the
# declaration is wrong. The only thing that catches it is asking the gate to say
# what it did, which is what `gate.proof` is for.
b="$(fresh_copy false-green)"
write "$b/bin/prime" <<'SH'
#!/usr/bin/env bash
# A well-formed gate that runs nothing, prints nothing and exits 0. This is the
# false green reproduced deliberately: every declaration is true about it.
exit 0
SH
chmod +x "$b/bin/prime"
expect_red 'a declaration that is entirely true about a gate that exited 0 without running anything' \
  "$b" 'gate.proof-missing' --prove

# Runs, prints every declared proof, and then fails. Without this the checker
# would accept a gate that proved itself and died on the next line.
b="$(fresh_copy nonzero)"
install_standin "$b"
printf 'echo "FAIL something after the summary" >&2\nexit 1\n' >> "$b/bin/prime"
expect_red 'a gate that ran, printed its proofs, and still failed' "$b" 'gate.nonzero' --prove

# A budget so small the gate cannot finish inside it, and WITHOUT a sleep:
# `sleep 3` would make the result depend on the scheduler rather than on the
# checker. A CPU-bound wait ends by itself, so a checker that failed to stop the
# gate makes this script slow rather than hung.
b="$(fresh_copy timeout)"
edit "$b/gate.yml" 'timeoutSeconds: 1800' 'timeoutSeconds: 1'
write "$b/bin/prime" <<'SH'
#!/usr/bin/env bash
set -uo pipefail
"$PYTHON" - <<'PYEOF'
import time

end = time.monotonic() + 3.0
while time.monotonic() < end:
    pass
PYEOF
SH
"$PY" - "$b/bin/prime" <<PY
import sys
path = sys.argv[1]
body = open(path).read().replace('"\$PYTHON"', repr("$PY"))
open(path, "w").write(body)
PY
chmod +x "$b/bin/prime"
expect_red 'a gate that outlived the budget its own declaration gave it' "$b" 'gate.timeout' --prove

# A floor is a decrease-detector, and this is what proves it is one rather than
# decoration: the pattern compiles, the gate is true, and the declaration is red
# because there is no capture group to read the number from. Only the proving
# phase can see it, so the stand-in matters here — this finding sits BEHIND a
# successful match, so the gate has to actually print the line.
b="$(fresh_copy proof-unmeasurable)"
install_standin "$b"
edit "$b/gate.yml" "match: '^caf: ([0-9]+) packages, [0-9]+ top-level passes" \
  "match: '^caf: [0-9]+ packages, [0-9]+ top-level passes"
expect_red 'a floor with no capture group to read it from, against a gate that ran and printed its proof' \
  "$b" 'gate.proof-invalid' --prove

# The floor, against a gate that reports FEWER tests than the declaration
# promised. The stand-in prints 40 where the floor is well above it, and
# `gate.floor` is what the checker says. The 40 is a literal on purpose — it is
# the stand-in's one caller that passes a count, and it must stay far below
# whatever the floor currently is.
#
# A stand-in rather than the real gate, and the reason is worth recording
# because it took a real run to find: this case cannot use the real
# bin/prime. The copy it would run in is four files, so `go mod download` fails
# with "no modules specified" and the gate exits 1 having printed no summary at
# all. Copying the whole tree does not fix it either, and the reason is a
# pleasing one: `TestTheGateFloorIsNotAWall` lives IN the tree, so a copy whose
# gate.yml says `minimum: 999999` goes red on that test, the summary line then
# reads `1 failures`, and the three floor patterns — all of which require the
# literal `0 failures` — stop matching. The ratchet that keeps the floor honest
# is itself what stops this breakage from being staged against the real gate.
#
# Which is not a gap. The relationship between this floor and this repository's
# real test count is asserted from three other sides: control 2 above (the real
# gate prints 435 against a floor of 435 and is green), and the two
# `internal/ci` tests that read the tree. What is left for this case is the
# checker's own floor arithmetic, and a stand-in exercises that exactly.
b="$(fresh_copy floor)"
install_standin "$b" 40
expect_red 'a gate that proves fewer tests than the declaration promised' "$b" 'gate.floor' --prove

# The pattern itself, changed to something the real gate does not print. The
# same claim from the other side: "the declared proofs match what THIS
# repository's gate prints" is a claim about this gate, and it is the breakage
# that keeps the three floor patterns and the summary line in bin/prime honest
# from the declaration's side.
b="$(fresh_copy proof-unmatched)"
edit "$b/gate.yml" "subtest passes, 0 failures, [0-9]+ skips\$" "subtest passes, 0 failures, 999999 skips\$"
expect_red 'a proof that matches nothing the real gate prints' "$b" 'gate.proof-missing' --prove

# --------------------------------------------------------------------------
# THE TWO THAT ARE ONLY THIS REPOSITORY'S
# --------------------------------------------------------------------------

# `gate.proof[].live-tier` has no `minimum`, on purpose: a floor of 2 would make
# the declared gate red on every machine without a container runtime. So the
# proof's whole job is that the line EXISTS. Delete it from bin/prime and the
# declaration is red — a skip that a declaration requires to be stated cannot be
# a silent pass. This is the one breakage a stand-in cannot fake, because it is
# a claim about what the REAL gate prints.
b="$(fresh_copy live-tier-unaccounted)"
rm -f "$b/gate.yml"
cp "$ROOT/gate.yml" "$b/gate.yml"
"$PY" - "$b/bin/prime" <<'PY'
import sys

path = sys.argv[1]
lines = open(path, encoding="utf-8").read().splitlines(keepends=True)
kept, seen = [], 0
for line in lines:
    if "caf: live tier:" in line or line.startswith("case ") or line.startswith("  \"$LIVE_TIER\")") \
       or line.startswith("  0)") or line.startswith("  *)"):
        # Drop the whole reporting block, not just one printf, so the
        # breakage is "the line is not printed" rather than "one wording of
        # the line changed". A half-removed block would still print
        # something and the proof would still match.
        seen += 1
        continue
    if line.rstrip("\n") == "esac":
        continue
    kept.append(line)
if seen == 0:
    sys.exit("gate-declaration-self-test: no live-tier reporting block found in bin/prime")
open(path, "w", encoding="utf-8").write("".join(kept))
PY
if [ $? -ne 0 ]; then
  echo "FAIL gate-declaration-self-test: the live-tier breakage recipe no longer applies" >&2
  failures=$((failures + 1))
fi
expect_red 'a gate that no longer states how much of the live tier ran, so a skip is silent again' \
  "$b" 'gate.proof-missing' --prove

# And the assertion inside the gate, which is what makes `--live` a gate mode
# rather than a suggestion. Run the REAL bin/prime --live on a PATH with no
# container runtime on it, so the two live tests skip for the real reason they
# skip on a machine that has no docker — hermetically, with no sleep, and
# without starting or stopping anything.
#
# Against the REAL repository and not a copy, and the reason is the same one as
# the floor case above: a copy is four files, `go mod download` fails in it with
# "no modules specified", and the gate exits 1 before the live tier is reached
# — so the assertion under test is never evaluated and the case would pass for
# the wrong reason. `bin/prime` writes nothing into the repository (its log goes
# to `mktemp`, and `go build`/`go test` write to the build cache), so running it
# in place mutates nothing.
#
# A PATH is built out of symlinks rather than by hiding docker, because PATH
# entries are searched in order and a shadowing `docker` that is not executable
# is skipped in favour of the real one. The tool list is the one this
# repository's gate and its tests actually reach for; it was arrived at by
# running the gate with a short list and reading which tool it then missed
# (`touch`, from internal/cli's env test).
b="$(fresh_copy live-mode-refuses)"
shim="$b/.shim"
mkdir -p "$shim"
missing=""
for tool in go grep cat mktemp rm sed awk touch sh env sleep head tail cut sort uniq ls mkdir chmod cp date; do
  found="$(command -v "$tool" 2>/dev/null || true)"
  if [ -z "$found" ]; then missing="$missing $tool"; else ln -sf "$found" "$shim/$tool"; fi
done
if [ -n "$missing" ]; then
  skipped=$((skipped + 1))
  printf 'SKIP gate-declaration-self-test: the --live assertion — this machine has no%s, so a\n' "$missing"
  printf '     PATH without a container runtime cannot be built here. Everything else in this\n'
  printf '     script ran; this one did not, and a proof nobody ran is not a passing proof.\n'
else
  # The fast gate first, and it has to be GREEN under this PATH or the case
  # below proves nothing: a PATH too thin to run the suite at all would also
  # produce a nonzero exit and the missing live tier.
  shim_fast="$(PATH="$shim" "$ROOT/bin/prime" 2>&1)"
  shim_fast_code=$?
  if [ "$shim_fast_code" -ne 0 ]; then
    failures=$((failures + 1))
    printf 'FAIL gate-declaration-self-test: bin/prime is not green on a PATH built without a container\n' >&2
    printf '     runtime (%s), so the --live case below would pass for the wrong reason:\n%s\n' \
      "$shim_fast_code" "$shim_fast" >&2
  else
    live_out="$(PATH="$shim" "$ROOT/bin/prime" --live 2>&1)"
    live_code=$?
    if [ "$live_code" -eq 0 ]; then
      printf 'FAIL gate-declaration-self-test: bin/prime --live exited 0 with no container runtime on PATH.\n%s\n' \
        "$live_out" >&2
      failures=$((failures + 1))
    elif ! printf '%s' "$live_out" | grep -qF "the live tier ran 0 of $(live_tier_size) tests"; then
      printf 'FAIL gate-declaration-self-test: bin/prime --live went nonzero but never said the live tier did not run\n%s\n' \
        "$live_out" >&2
      failures=$((failures + 1))
    else
      breakages=$((breakages + 1))
      printf 'PASS gate-declaration-self-test: breakage %s: bin/prime --live with no container runtime — refused, and said so\n' \
        "$breakages"
    fi
  fi
fi

# --------------------------------------------------------------------------
printf '\n'
if [ "$failures" -ne 0 ]; then
  printf 'FAIL: gate-declaration-self-test — %s of %s cases failed.\n' "$failures" \
    "$((controls + breakages + warn_cases + failures))" >&2
  exit 1
fi
# The three "nothing ran" guards, and they are the reason a green line above is
# worth reading. A self-test whose recipes have gone stale reports zero
# breakages and exits 0, which is the exact failure this script exists to
# detect — a checker that has stopped checking, reported as a checker that
# passed. `edit` refusing an unmatched recipe catches it per-case; this catches
# the case where the whole file stopped running.
if [ "$breakages" -eq 0 ] || [ "$controls" -eq 0 ] || [ "$warn_cases" -eq 0 ]; then
  printf 'FAIL: gate-declaration-self-test — %s breakages, %s controls, %s warnings ran. A run with\n' \
    "$breakages" "$controls" "$warn_cases" >&2
  printf '  a zero in it proved nothing, not that nothing was wrong.\n' >&2
  exit 1
fi
# Every counter, separately, always. "15 passed" on its own could mean fifteen
# reds or ten reds and five controls; the point of this line is that a reader
# can tell which.
printf 'PASS: gate-declaration-self-test — %s controls green, %s breakages went red and each named the finding it was written for, %s warnings stayed green with their exit code at 0. %s skipped.\n' \
  "$controls" "$breakages" "$warn_cases" "$skipped"
