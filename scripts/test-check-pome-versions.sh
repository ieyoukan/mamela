#!/usr/bin/env bash
# Tests scripts/check-pome-versions.sh on small repositories made here: each case breaks one rule
# and must be refused with the reason, and the legitimate changes must pass. A check that cannot
# fail protects nothing, so this runs in scripts/check.sh next to the check itself.
#
# Usage: scripts/test-check-pome-versions.sh   (offline; needs only git and a temporary directory)
set -euo pipefail

checker="$(cd "$(dirname "$0")" && pwd)/check-pome-versions.sh"
tmp="$(mktemp -d)"
trap 'find "$tmp" -delete' EXIT

# Nothing of the user's git configuration (signing, hooks, templates) takes part.
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
g() { git -c commit.gpgsign=false -c tag.gpgsign=false -c init.defaultBranch=main "$@"; }

write() { mkdir -p "$(dirname "$1")" && printf '%s\n' "$2" >"$1"; }

# The repository module is example.invalid/repo. pome/lib is published as v0.1.0; pome/app depends
# on it (and its Pome.lock records the commit of the tag, as `emela pome install` does); the
# example uses the checkout of lib through [replace].
base="$tmp/base"
mkdir "$base"
cd "$base"
g init -q .
write pome/lib/Pome.toml '[pome]
name = "example.invalid/repo/pome/lib"
version = "0.1.0"
emela = "0.27.0"
module = "Lib"

[dependencies]
"example.invalid/other" = "v1.0.0"'
write pome/lib/src/lib.emel 'pub fn one() { 1 }'
write pome/lib/test/lib_test.emel '@test fn t() { }'
write pome/lib/Pome.lock 'version = 1'
write examples/ex/Pome.toml '[pome]
name = "example.invalid/repo/examples/ex"
version = "0.1.0"
emela = "0.27.0"

[dependencies]
"example.invalid/repo/pome/lib" = "v0.1.0"

[replace]
"example.invalid/repo/pome/lib" = { path = "../../pome/lib" }'
write internal/evaluate/testdata/stale/Pome.toml '[pome]
name = "example.invalid/stale"
version = "0.1.0"

[dependencies]
"example.invalid/repo/pome/lib" = "v0.0.1"

[replace]
"example.invalid/repo/pome/lib" = { path = "../../../../pome/lib" }'
g add -A
g commit -q -m "lib 0.1.0"
g tag -a pome/lib/v0.1.0 -m "Lib Pome 0.1.0"
write pome/app/Pome.toml '[pome]
name = "example.invalid/repo/pome/app"
version = "0.1.0"
emela = "0.27.0"
module = "App"

[dependencies]
"example.invalid/repo/pome/lib" = "v0.1.0"'
write pome/app/Pome.lock "version = 1

[[pome]]
path = \"example.invalid/repo/pome/lib\"
version = \"v0.1.0\"
commit = \"$(g rev-parse HEAD)\"
hash = \"h1:x\""
g add -A
g commit -q -m "app 0.1.0"

cases=0
# expect <name> <pass|fail> [text the report must contain]...: runs the checker in $dir.
expect() {
  local name="$1" want="$2" got=pass
  shift 2
  if "$checker" "$dir" >"$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $name: expected the check to $want, but it did $got" >&2; cat "$tmp/out" >&2; exit 1
  fi
  local text
  for text in "$@"; do
    if ! grep -qF -- "$text" "$tmp/out"; then
      echo "FAIL $name: the report does not say \"$text\"" >&2; cat "$tmp/out" >&2; exit 1
    fi
  done
  cases=$((cases + 1))
  echo "ok: $name"
}
# fresh <name>: a copy of the base repository to break.
fresh() { dir="$tmp/case-$1"; cp -R "$base" "$dir"; cd "$dir"; }
# sub <file> <regexp> <replacement>: edits a file of the copy ('|' delimits).
sub() { sed -i.bak "s|$2|$3|" "$1" && find . -name '*.bak' -not -path './.git/*' -delete; }
bump() { sub "$2" '^version = "[0-9.]*"' "version = \"$1\""; }
use_lib() { sub examples/ex/Pome.toml 'pome/lib" = "v[0-9.]*"' "pome/lib\" = \"v$1\""; }

fresh clean
expect "a clean repository passes" pass "agree"

# --- a published version names one content -------------------------------------------------------
fresh changed-source
echo 'pub fn two() { 2 }' >>pome/lib/src/lib.emel
expect "source changed, version not raised" fail "differ from the tag pome/lib/v0.1.0" "src/lib.emel" "raise version"

fresh changed-source-committed
echo 'pub fn two() { 2 }' >>pome/lib/src/lib.emel
g commit -q -am "change"
expect "the same, after it is committed (CI sees it so)" fail "differ from the tag pome/lib/v0.1.0" "src/lib.emel"

fresh new-file
write pome/lib/src/extra.emel 'pub fn x() { 1 }'
expect "a new file, not yet added to git" fail "src/extra.emel"

fresh removed-file
g rm -q pome/lib/src/lib.emel
expect "a removed file" fail "src/lib.emel"

fresh changed-dependency
sub pome/lib/Pome.toml 'v1.0.0' 'v1.1.0'
expect "[dependencies] changed, version not raised" fail "Pome.toml" "differ from the tag"

fresh test-and-lock-only
echo '@test fn u() { }' >>pome/lib/test/lib_test.emel
echo '# relocked' >>pome/lib/Pome.lock
expect "tests and Pome.lock may change under a published version" pass

fresh raised
bump 0.1.1 pome/lib/Pome.toml
echo 'pub fn two() { 2 }' >>pome/lib/src/lib.emel
expect "a raised version with changed source, example still on the old one" fail \
  "[dependencies] requires example.invalid/repo/pome/lib v0.1.0" "set it to \"v0.1.1\""
if grep -qF "differ from the tag" "$tmp/out"; then echo "FAIL: a version without a tag was compared with another tag" >&2; exit 1; fi
use_lib 0.1.1
expect "the same with the example raised" pass

fresh changing-the-version-being-prepared
bump 0.1.1 pome/lib/Pome.toml
use_lib 0.1.1
g commit -q -am "prepare 0.1.1"
echo 'pub fn two() { 2 }' >>pome/lib/src/lib.emel
expect "an untagged version may keep changing until it is tagged" pass

# --- tags ------------------------------------------------------------------------------------------
fresh version-behind-the-tags
bump 0.1.1 pome/lib/Pome.toml
use_lib 0.1.1
bump 0.2.0 pome/lib/Pome.toml
g commit -q -am "lib 0.2.0"
g tag -a pome/lib/v0.2.0 -m "Lib Pome 0.2.0"
bump 0.1.1 pome/lib/Pome.toml
expect "Pome.toml older than a published version" fail "version 0.1.1 is older than the published pome/lib/v0.2.0"

fresh tag-disagrees-with-pome-toml
g tag -a pome/lib/v0.2.0 -m "wrong"
expect "a tag whose Pome.toml says another version" fail "the tag names version 0.2.0, but its Pome.toml says \"0.1.0\""

fresh lightweight-tag
bump 0.1.1 pome/lib/Pome.toml
use_lib 0.1.1
g commit -q -am "lib 0.1.1"
g tag pome/lib/v0.1.1
expect "a lightweight tag" fail "pome/lib/v0.1.1: the tag is lightweight"

fresh malformed-tag
g tag -a pome/lib/v1 -m "oops"
expect "a tag that is not vMAJOR.MINOR.PATCH" fail "pome/lib/v1: the tag is not named"

fresh moved-tag
echo 'pub fn two() { 2 }' >>pome/lib/src/lib.emel
g commit -q -am "change after the tag"
g tag -f -a pome/lib/v0.1.0 -m "moved" >/dev/null
expect "a tag moved after a consumer fetched it" fail "was moved after it was fetched" "restore the tag"

fresh shallow
dir="$tmp/case-shallow-clone"
g clone -q --depth 1 "file://$tmp/case-shallow" "$dir"
cd "$dir"
expect "a shallow clone (no tags to read)" fail "shallow clone"

# --- dependencies of Pomes on Pomes of this repository -----------------------------------------------
fresh unpublished-dependency
sub pome/app/Pome.toml 'pome/lib" = "v0.1.0"' 'pome/lib" = "v0.9.0"'
expect "a Pome that requires an unpublished version of another" fail "has no tag pome/lib/v0.9.0" "publish pome/lib first"

# --- names and [replace] ---------------------------------------------------------------------------------
fresh name-and-directory
sub pome/app/Pome.toml 'repo/pome/app' 'repo/pome/other'
expect "a Pome whose name is not its directory" fail "does not end in its directory pome/app"

fresh replace-to-nowhere
sub examples/ex/Pome.toml '../../pome/lib' '../../pome/nowhere'
expect "[replace] to a directory that does not exist" fail "has no Pome.toml" "fix the path"

fresh replace-to-another-pome
sub examples/ex/Pome.toml '../../pome/lib' '../../pome/app'
expect "[replace] to a different Pome" fail "which is the Pome \"example.invalid/repo/pome/app\""

fresh new-pome
write pome/fresh/Pome.toml '[pome]
name = "example.invalid/repo/pome/fresh"
version = "0.1.0"
emela = "0.27.0"
module = "Fresh"'
write pome/fresh/src/lib.emel 'pub fn f() { 1 }'
expect "a Pome with no tag yet (the first version)" pass

echo "$cases cases ok"
