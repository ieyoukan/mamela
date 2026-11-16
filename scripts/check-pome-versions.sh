#!/usr/bin/env bash
# Checks that the versions of this repository's Pomes mean what they say (docs/releasing.md). Reads
# only the working tree and git (the tags and their objects): no network, no toolchain.
#
# For every published Pome (pome/<name>, tagged pome/<name>/vX.Y.Z):
#   - each tag is annotated, and the Pome.toml at the tag says the version the tag names;
#   - a version that has a tag has the contents of that tag: a changed Pome needs a new version in
#     the same PR (a version that has no tag yet may still change; it is the one being prepared);
#   - Pome.toml does not say a version older than a published one;
#   - a dependency on another Pome of this repository names a published version, and the tag is
#     still the commit that Pome.lock recorded when the dependency was fetched (no moved tag).
# For every Pome.toml with a [replace] path (the examples):
#   - the path holds the Pome the entry names, and [dependencies] requires exactly its version, so
#     an example cannot use an API that the version it names does not have.
# Contents are every file of the Pome except test/ and Pome.lock, which a user of the Pome never
# builds. The tags are read from the clone, so a shallow one (no tags) is refused.
#
# Usage: scripts/check-pome-versions.sh [repository-root]   (scripts/test-check-pome-versions.sh tests it)
set -euo pipefail

root="$(cd "${1:-$(dirname "$0")/..}" && pwd)"
cd "$root"

failures=0
fail() { # fail <where> <what> <why> <fix>
  printf '%s: %s\n  why: %s\n  fix: %s\n' "$1" "$2" "$3" "$4" >&2
  failures=$((failures + 1))
}

# toml_value <file or -> <section> <key>: the string value of key in [section] (bare or quoted key).
toml_value() {
  awk -v sec="$2" -v key="$3" '
    /^[ \t]*\[/ { s = $0; sub(/^[ \t]*\[/, "", s); sub(/\][ \t]*(#.*)?$/, "", s); in_sec = (s == sec); next }
    in_sec {
      line = $0; sub(/^[ \t]+/, "", line)
      k = line; sub(/[ \t]*=.*/, "", k); gsub(/"/, "", k)
      if (k != key) next
      v = line; sub(/^[^=]*=[ \t]*/, "", v)
      if (match(v, /^"[^"]*"/)) { print substr(v, RSTART + 1, RLENGTH - 2); exit }
    }' "$1"
}

# toml_keys <file> <section>: the keys of [section], quoted or not.
toml_keys() {
  awk -v sec="$2" '
    /^[ \t]*\[/ { s = $0; sub(/^[ \t]*\[/, "", s); sub(/\][ \t]*(#.*)?$/, "", s); in_sec = (s == sec); next }
    in_sec && /^[ \t]*[^ \t#]/ { k = $0; sub(/^[ \t]+/, "", k); sub(/[ \t]*=.*/, "", k); gsub(/"/, "", k); print k }' "$1"
}

# replace_paths <file>: "<dependency><TAB><path>" for each `"dependency" = { path = "..." }` in [replace].
replace_paths() {
  awk '
    /^[ \t]*\[/ { s = $0; sub(/^[ \t]*\[/, "", s); sub(/\][ \t]*(#.*)?$/, "", s); in_sec = (s == "replace"); next }
    in_sec && /path[ \t]*=[ \t]*"/ {
      n = $0; sub(/^[ \t]*"/, "", n); sub(/".*/, "", n)
      p = $0; sub(/.*path[ \t]*=[ \t]*"/, "", p); sub(/".*/, "", p)
      printf "%s\t%s\n", n, p
    }' "$1"
}

# locked_commit <lock file> <path> <version>: the commit Pome.lock recorded for a dependency.
locked_commit() {
  awk -v want_path="$2" -v want_ver="$3" '
    function flush() { if (path == want_path && ver == want_ver && commit != "") { print commit; found = 1 } }
    /^\[\[pome\]\]/ { if (!found) flush(); path = ""; ver = ""; commit = ""; next }
    /^path = "/    { path = $0; sub(/^path = "/, "", path); sub(/".*/, "", path) }
    /^version = "/ { ver = $0; sub(/^version = "/, "", ver); sub(/".*/, "", ver) }
    /^commit = "/  { commit = $0; sub(/^commit = "/, "", commit); sub(/".*/, "", commit) }
    END { if (!found) flush() }' "$1" | head -1
}

# version_key <X.Y.Z>: a string that sorts like the version.
version_key() { awk -F. '{ printf "%09d%09d%09d", $1, $2, $3 }' <<<"$1"; }
is_version() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; }

# Contents of a Pome: "<blob> <path in the Pome>" lines, without test/ and Pome.lock. At a tag, or
# (no tag) in the working tree, where files not yet added count too: this is what `git add -A` would
# commit, so the check says the same before and after committing.
excluded() { [[ "$1" == test/* || "$1" == Pome.lock ]]; }
content_at_tag() {
  git ls-tree -r --full-tree "refs/tags/$1" -- "$2" | while IFS=$'\t' read -r meta path; do
    rel="${path#"$2"/}"
    excluded "$rel" && continue
    read -r _ _ blob <<<"$meta"
    printf '%s %s\n' "$blob" "$rel"
  done | LC_ALL=C sort
}
content_now() {
  git ls-files --cached --others --exclude-standard -- "$1" | LC_ALL=C sort -u | while IFS= read -r path; do
    [[ -f "$path" ]] || continue
    rel="${path#"$1"/}"
    excluded "$rel" && continue
    printf '%s %s\n' "$(git hash-object -- "$path")" "$rel"
  done | LC_ALL=C sort
}

if [[ "$(git rev-parse --is-shallow-repository)" == true ]]; then
  fail "git" "this is a shallow clone, so the tags of the Pomes cannot be read" \
    "a version is checked against its tag, and a missing tag would look like an unpublished version" \
    "fetch the whole history and the tags (git fetch --unshallow --tags; in CI: actions/checkout with fetch-depth: 0)"
  exit 1
fi

# --- the published Pomes: tags, Pome.toml and contents --------------------------------------------
for dir in pome/*/; do
  dir="${dir%/}"
  toml="$dir/Pome.toml"
  [[ -f "$toml" ]] || continue
  name="$(toml_value "$toml" pome name)"
  version="$(toml_value "$toml" pome version)"
  if [[ -z "$name" || "$name" != */"$dir" ]]; then
    fail "$toml" "the Pome's name \"$name\" does not end in its directory $dir" \
      "the tag pome/<name>/vX.Y.Z is the path of the directory, and a consumer fetches the Pome by its name" \
      "name the Pome <module path of the repository>/$dir"
    continue
  fi
  if ! is_version "$version"; then
    fail "$toml" "version \"$version\" is not MAJOR.MINOR.PATCH" \
      "tags are named vMAJOR.MINOR.PATCH and compared as numbers" "write the version as 0.3.2"
    continue
  fi
  repo_module="${name%/"$dir"}"

  latest="" latest_key=""
  while IFS= read -r tag; do
    [[ -n "$tag" ]] || continue
    tag_version="${tag#"$dir"/v}"
    if ! is_version "$tag_version"; then
      fail "$tag" "the tag is not named $dir/vMAJOR.MINOR.PATCH" \
        "a consumer asks for exactly the version tag of a Pome, and a tag that is none of them is a mistake" \
        "delete the tag locally and on the remote if it was pushed by mistake"
      continue
    fi
    if [[ "$(git cat-file -t "refs/tags/$tag")" != tag ]]; then
      fail "$tag" "the tag is lightweight" \
        "a published version is a record: who tagged it, when, and why must be in the tag (docs/releasing.md)" \
        "tag with: git tag -a $tag -m \"...\" <merge commit>"
    fi
    at_tag="$(git show "refs/tags/$tag:$toml" 2>/dev/null | toml_value - pome version || true)"
    if [[ "$at_tag" != "$tag_version" ]]; then
      fail "$tag" "the tag names version $tag_version, but its Pome.toml says \"$at_tag\"" \
        "consumers get what the tag holds, and Pome.toml is what the Pome says it is" \
        "a published tag cannot be fixed: publish a new version whose Pome.toml and tag agree (docs/releasing.md)"
    fi
    key="$(version_key "$tag_version")"
    if [[ -z "$latest" || "$key" > "$latest_key" ]]; then latest="$tag_version"; latest_key="$key"; fi
  done < <(git tag --list "$dir/v*")

  if git rev-parse -q --verify "refs/tags/$dir/v$version" >/dev/null; then
    changed="$(LC_ALL=C comm -3 <(content_at_tag "$dir/v$version" "$dir") <(content_now "$dir") | sed -e 's/^[[:space:]]*//' -e 's/^[^ ]* //' | LC_ALL=C sort -u | tr '\n' ' ')"
    if [[ -n "${changed// /}" ]]; then
      fail "$dir" "the contents differ from the tag $dir/v$version, but Pome.toml still says version $version (changed: ${changed% })" \
        "a published version names one content for good: what the tag holds is what consumers, their Pome.lock and the sumdb have" \
        "raise version in $toml in the same PR (and the examples' [dependencies] with it); docs/releasing.md"
    fi
  elif [[ -n "$latest" && "$(version_key "$version")" < "$latest_key" ]]; then
    fail "$toml" "version $version is older than the published $dir/v$latest" \
      "a version never goes back: consumers who took v$latest would be told this is older, with different contents" \
      "raise version above $latest (this branch may be based on an old main: git rebase origin/main)"
  fi

  # Dependencies on Pomes of this repository must be published: nothing replaces them for a consumer.
  while IFS= read -r dep; do
    [[ "$dep" == "$repo_module"/pome/* ]] || continue
    dep_dir="${dep#"$repo_module"/}"
    dep_version="$(toml_value "$toml" dependencies "$dep")"
    dep_tag="$dep_dir/$dep_version"
    if ! git rev-parse -q --verify "refs/tags/$dep_tag" >/dev/null; then
      fail "$toml" "requires $dep $dep_version, which has no tag $dep_tag" \
        "a consumer fetches the dependency by its tag; a Pome that names an unpublished version cannot be built" \
        "publish $dep_dir first (docs/releasing.md), then raise this requirement"
      continue
    fi
    locked="$(locked_commit "$dir/Pome.lock" "$dep" "$dep_version")"
    if [[ -n "$locked" && "$locked" != "$(git rev-parse "refs/tags/$dep_tag^{commit}")" ]]; then
      fail "$dir/Pome.lock" "the tag $dep_tag points to $(git rev-parse --short "refs/tags/$dep_tag^{commit}"), but Pome.lock fetched $dep_version at ${locked:0:7}" \
        "the tag was moved after it was fetched: the same version now names other contents" \
        "restore the tag to ${locked:0:7} (git tag -f -a ... then push the tag), or publish the change as a new version"
    fi
  done < <(toml_keys "$toml" dependencies)
done

# --- [replace] paths: the examples use a checkout, so their requirement is what proves the API ----
while IFS= read -r toml; do
  case "$toml" in internal/evaluate/testdata/*) continue ;; esac
  [[ -f "$toml" ]] || continue
  from="$(dirname "$toml")"
  while IFS=$'\t' read -r dep path; do
    [[ -n "$dep" ]] || continue
    target="$from/$path"
    if [[ ! -f "$target/Pome.toml" ]]; then
      fail "$toml" "[replace] sends $dep to $path, which has no Pome.toml" \
        "a [replace] path is relative to this file, and nothing is there" \
        "fix the path, or remove the entry to use the published version"
      continue
    fi
    target_name="$(toml_value "$target/Pome.toml" pome name)"
    if [[ "$target_name" != "$dep" ]]; then
      fail "$toml" "[replace] sends $dep to $path, which is the Pome \"$target_name\"" \
        "the path must hold the Pome that the entry names" "point the entry at the right directory"
      continue
    fi
    target_version="$(toml_value "$target/Pome.toml" pome version)"
    required="$(toml_value "$toml" dependencies "$dep")"
    [[ -n "$required" ]] || continue # replaces a dependency of a dependency: that Pome states its own
    if [[ "$required" != "v$target_version" ]]; then
      fail "$toml" "[dependencies] requires $dep $required, but [replace] uses the checkout $path, whose version is $target_version" \
        "the checkout hides the requirement: this would build against API that $required does not have, and only users of the published version would find out" \
        "set it to \"v$target_version\" (docs/releasing.md)"
    fi
  done < <(replace_paths "$toml")
done < <(git ls-files --cached --others --exclude-standard -- '*Pome.toml')

if [[ "$failures" != 0 ]]; then
  printf '\n%d problem(s) with Pome versions\n' "$failures" >&2
  exit 1
fi
echo "Pome versions, tags and examples agree"
