#!/usr/bin/env bash
# Shows that the checks beside this file notice what they are for: each
# case adds one file to the index of a throwaway copy of the repository and
# expects the check to fail. A check that passes on a clean tree only
# because it cannot fail is what this guards against.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
scripts="$root/.github/scripts"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The copy is made of the committed files, and has an index of its own.
mkdir "$work/repo"
git -C "$root" ls-files -z | (cd "$root" && xargs -0 tar -cf - 2>/dev/null) | tar -xf - -C "$work/repo"
git -C "$work/repo" init --quiet
git -C "$work/repo" add --all

failures=0
# must_fail <check> <path> <content>: the check fails once the file is staged.
must_fail() {
  local check="$1" path="$2" content="$3"
  mkdir -p "$work/repo/$(dirname "$path")"
  printf '%b' "$content" > "$work/repo/$path"
  git -C "$work/repo" add --force -- "$path"
  if (cd "$work/repo" && "$scripts/$check" > "$work/output" 2>&1); then
    echo "FAIL  $check passed with $path committed"
    failures=$((failures + 1))
  else
    echo "ok    $check refuses $path: $(grep -m 1 '::error' "$work/output" || tail -n 1 "$work/output")"
  fi
  git -C "$work/repo" rm --quiet --cached --force -- "$path"
  rm "$work/repo/$path"
}

for check in check-sensitive-files.sh check-yaml.sh; do
  if ! (cd "$work/repo" && "$scripts/$check" > "$work/output" 2>&1); then
    echo "FAIL  $check fails on the committed files:"
    cat "$work/output"
    failures=$((failures + 1))
  else
    echo "ok    $check passes on the committed files: $(tail -n 1 "$work/output")"
  fi
done

must_fail check-sensitive-files.sh .env 'DATABASE_PASSWORD=x\n'
must_fail check-sensitive-files.sh deploy/.env.production 'TOKEN=x\n'
must_fail check-sensitive-files.sh deploy/tls/server.key 'not even a key\n'
must_fail check-sensitive-files.sh home/.ssh/id_ed25519 'x\n'
# The marker is put together here so that this file holds no key material.
must_fail check-sensitive-files.sh docs/notes.md "-----BEGIN OPENSSH PRIVATE"" KEY-----\nAAAA\n"
must_fail check-sensitive-files.sh internal/x/fixture.txt "-----BEGIN PRIVATE"" KEY-----\nAAAA\n"
must_fail check-yaml.sh examples/broken.yaml 'a: [1, 2\n'
must_fail check-yaml.sh examples/second-document.yml 'a: 1\n---\nb: {\n'

# What must stay allowed: a template of an environment file, and text that
# only mentions such files.
printf 'TOKEN=\n' > "$work/repo/.env.example"
printf 'Copy .env.example to .env; never commit PRIVATE_KEY or AWS_SECRET values.\n' > "$work/repo/NOTES.md"
git -C "$work/repo" add --force -- .env.example NOTES.md
if (cd "$work/repo" && "$scripts/check-sensitive-files.sh" > "$work/output" 2>&1); then
  echo "ok    check-sensitive-files.sh allows .env.example and text that mentions .env"
else
  echo "FAIL  check-sensitive-files.sh refuses a template or a mention:"
  cat "$work/output"
  failures=$((failures + 1))
fi

if [ "$failures" -ne 0 ]; then
  echo "$failures cases failed"
  exit 1
fi
