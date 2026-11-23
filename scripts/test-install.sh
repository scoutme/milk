#!/bin/sh
# Regression harness for the install scripts — issue #440:
# `curl -fsSL .../install.sh | sh` used to print nothing and install nothing
# on Linux/macOS, because detect_platform() ended in `[ ... ] && EXT=...`,
# returned 1 when the test was false, and main called it as a plain command
# under `set -e` — the script died before its first message.
#
# Runs the real install.sh in a sandbox (fake HOME, stub curl/wget, never the
# network, never a real install) and asserts it survives `set -e` on every
# available POSIX shell, reaches its messages, installs the stub binary, and
# still works when no sha256 tool exists (the same `[ ... ] && success` idiom
# at the checksum branch, fixed in the same pass).
#
# Usage: sh scripts/test-install.sh     (from anywhere; repo-root derived)

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
ORIGPATH="$PATH"
FAILED=0
NOTES=0

note() { NOTES=$((NOTES + 1)); printf ' %s\n' "$*"; }
fail() { FAILED=1; printf 'FAIL: %s\n' "$*" >&2; }

# ── 1. syntax: both scripts parse under sh -n and bash -n ────────────────────
for s in install.sh install-from-source.sh; do
    if sh -n "$ROOT/$s" 2>/dev/null; then
        note "syntax ok (sh -n): $s"
    else
        fail "sh -n rejected $s"
    fi
done
if command -v bash >/dev/null 2>&1; then
    for s in install.sh install-from-source.sh; do
        if bash -n "$ROOT/$s" 2>/dev/null; then
            note "syntax ok (bash -n): $s"
        else
            fail "bash -n rejected $s"
        fi
    done
fi

# ── 2. sandbox: fake HOME, stub curl/wget, never the network ─────────────────
SANDBOX=$(mktemp -d)
trap 'rm -rf "$SANDBOX"' EXIT
mkdir -p "$SANDBOX/home" "$SANDBOX/stub" "$SANDBOX/nosha"

# Content + digest the stub curl serves for any binary download. Computed here
# (not hardcoded) and exported so the stub needs no hashing tools of its own —
# scenario "nosha" runs with a PATH that deliberately lacks sha256sum/shasum.
FAKE_BIN='fake-milk-binary'
STUB_SHA=$(printf '%s\n' "$FAKE_BIN" | sha256sum | awk '{print $1}')
export FAKE_BIN STUB_SHA

# curl stub: serves the releases/latest tag on stdout, writes the binary and
# its .sha256 through -o, refuses anything else (exit 22) so an unexpected
# URL fails the scenario instead of silently succeeding.
cat > "$SANDBOX/stub/curl" <<'STUB'
#!/bin/sh
out=""
url=""
while [ $# -gt 0 ]; do
    case "$1" in
        -o) out="$2"; shift 2;;
        -*) shift;;
        *)  url="$1"; shift;;
    esac
done
case "$url" in
    */releases/latest)
        printf '  "tag_name": "v9.9.9",\n';;
    *.sha256)
        if [ -n "$out" ]; then printf '%s  milk\n' "$STUB_SHA" > "$out"
        else printf '%s  milk\n' "$STUB_SHA"; fi;;
    */download/*)
        if [ -n "$out" ]; then printf '%s\n' "$FAKE_BIN" > "$out"
        else printf '%s\n' "$FAKE_BIN"; fi;;
    *) exit 22;;
esac
STUB
chmod +x "$SANDBOX/stub/curl"
cp "$SANDBOX/stub/curl" "$SANDBOX/stub/wget"  # never selected when curl answers; loud if reached

# "nosha" PATH: everything the script runs after download, except sha tools —
# pins the checksum-branch's empty-ACTUAL path (install must still succeed).
for c in uname grep head sed mktemp awk mkdir mv chmod rm basename printf echo; do
    p=$(command -v "$c" 2>/dev/null) || { fail "host tool missing: $c"; continue; }
    ln -sf "$p" "$SANDBOX/nosha/$c"
done
cp "$SANDBOX/stub/curl" "$SANDBOX/nosha/curl"
# a sha tool must genuinely be absent for this scenario:
if [ -e "$SANDBOX/nosha/sha256sum" ] || [ -e "$SANDBOX/nosha/shasum" ]; then
    fail "nosha dir accidentally contains a sha tool"
fi

# ── 3. runner ────────────────────────────────────────────────────────────────
# $1 = shell invocation (word-split on purpose: "dash", "bash", "busybox sh")
# $2 = PATH for the run, $3 = label, remaining env via $4/$5 (name/value pairs
# are caller-built into the env -i prefix through $EXTRA).
run_install() {
    # resolve the shell to an absolute path BEFORE env -i wipes PATH lookup
    # (and the stub's inputs: env -i keeps only what is passed right here)
    _first=${1%% *}
    _rest=${1#* }
    [ "$_rest" = "$1" ] && _rest=""
    _abs=$(command -v "$_first" 2>/dev/null) || { fail "shell not found: $_first"; STATUS=127; OUT=""; return 0; }
    _path=$2; _extra=$4
    OUT=$(env -i HOME="$SANDBOX/home" SHELL=/bin/bash TERM=dumb \
        PATH="$_path" FAKE_BIN="$FAKE_BIN" STUB_SHA="$STUB_SHA" $_extra \
        "$_abs" $_rest "$ROOT/install.sh" 2>&1)
    STATUS=$?
    return 0
}

assert_ok() {  # $1 = label: exit 0 + reached detect + installed + checksum verified
    _l=$1
    if [ "$STATUS" -eq 0 ]; then note "$_l: exit 0"; else fail "$_l: exit $STATUS"; fi
    if [ -z "$OUT" ]; then
        fail "$_l: EMPTY output — issue #440 reproduced (silent set -e death before first message)"
        return
    fi
    printf '%s' "$OUT" | grep -q "Resolving latest release" \
        && note "$_l: passed detect_platform (first message printed)" \
        || fail "$_l: never printed 'Resolving latest release' — died before resolve_version"
    printf '%s' "$OUT" | grep -q "Checksum verified" \
        && note "$_l: checksum branch verified" \
        || fail "$_l: no 'Checksum verified'"
    printf '%s' "$OUT" | grep -q "Installed milk v9.9.9" \
        && note "$_l: install completed" \
        || fail "$_l: no 'Installed milk v9.9.9'"
    if [ -x "$SANDBOX/home/.local/bin/milk" ]; then
        _got=$(cat "$SANDBOX/home/.local/bin/milk")
        [ "$_got" = "$FAKE_BIN" ] \
            && note "$_l: binary landed with stub content" \
            || fail "$_l: unexpected binary content: $_got"
    else
        fail "$_l: $SANDBOX/home/.local/bin/milk missing or not executable"
    fi
}

# ── 4. scenario A: happy path, every available shell ─────────────────────────
# comma-separated so a two-word entry ("busybox sh") survives the loop
SHELLS="sh,dash"
command -v bash >/dev/null 2>&1 && SHELLS="$SHELLS,bash"
command -v busybox >/dev/null 2>&1 && SHELLS="$SHELLS,busybox sh"

OLDIFS=$IFS; IFS=,
for shcmd in $SHELLS; do
    IFS=$OLDIFS
    label="A[$shcmd]"
    run_install "$shcmd" "$SANDBOX/stub:$ORIGPATH" "$label" ""
    assert_ok "$label"
    rm -rf "$SANDBOX/home/.local"
done
IFS=$OLDIFS

# ── 5. scenario B: no sha tool in PATH (empty ACTUAL) ────────────────────────
run_install dash "$SANDBOX/nosha" "B[dash nosha]" ""
if [ "$STATUS" -eq 0 ]; then
    note "B: exit 0 with no sha256 tool"
else
    fail "B: exit $STATUS with no sha256 tool (checksum branch died)"
fi
printf '%s' "$OUT" | grep -q "Installed milk v9.9.9" \
    && note "B: install completed without checksum tool" \
    || fail "B: no 'Installed milk' — output: $OUT"
printf '%s' "$OUT" | grep -q "Checksum verified" \
    && fail "B: claimed checksum verification with no sha tool" \
    || note "B: no false 'Checksum verified'"
rm -rf "$SANDBOX/home/.local"

# ── 6. scenario C: MILK_VERSION short-circuits version resolution ────────────
run_install dash "$SANDBOX/stub:$ORIGPATH" "C[dash MILK_VERSION]" "MILK_VERSION=v1.2.3"
printf '%s' "$OUT" | grep -q "Downloading milk v1.2.3" \
    && note "C: MILK_VERSION honored" \
    || fail "C: MILK_VERSION ignored — output: $OUT"
printf '%s' "$OUT" | grep -q "Resolving latest release" \
    && fail "C: still hit version resolution despite MILK_VERSION" \
    || note "C: version resolution skipped"
[ "$STATUS" -eq 0 ] || fail "C: exit $STATUS"
[ -x "$SANDBOX/home/.local/bin/milk" ] || fail "C: binary missing"

# ── summary ──────────────────────────────────────────────────────────────────
if [ "$FAILED" -eq 0 ]; then
    printf 'install-script harness: PASS (%d checks)\n' "$NOTES"
    exit 0
fi
printf 'install-script harness: FAILURES ABOVE\n' >&2
exit 1
