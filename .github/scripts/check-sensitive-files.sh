#!/usr/bin/env bash
# Fails when a file that should not be in the repository is committed:
# a file that is sensitive by its name (an environment file, a private key,
# a key store), or any text file that holds private key material.
#
# It reads the index, so it judges what is committed or staged and not what
# merely lies in the working tree. Run it from anywhere in the checkout:
#
#   .github/scripts/check-sensitive-files.sh
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Names, matched against the last part of the path without regard to case.
# An environment file that is a template (.env.example and the like) holds
# no values and is allowed.
by_name='^(\.env(\..+)?|.+\.(pem|key|p12|pfx|jks|keystore|ppk)|id_(rsa|dsa|ecdsa|ed25519))$'
template='^\.env\.(example|sample|template|dist)$'

found=0
while IFS= read -r -d '' path; do
  name="${path##*/}"
  if printf '%s\n' "$name" | grep -Eiq "$by_name" && ! printf '%s\n' "$name" | grep -Eiq "$template"; then
    echo "::error file=${path}::sensitive by name: ${path}"
    found=1
  fi
done < <(git ls-files -z)

# Content: the first line of a PEM or OpenSSH private key. The expression
# does not match its own text, so this file passes.
key_material='-----BEGIN [A-Z0-9 ]*PRIVATE KEY( BLOCK)?-----'
# git grep exits 1 when nothing matches and above 1 when it could not
# search; only the first is a pass.
status=0
matches="$(git grep --cached -I -l -E -e "$key_material" -- .)" || status=$?
if [ "$status" -gt 1 ]; then
  echo "::error::git grep failed with status ${status}; the contents were not checked"
  exit "$status"
fi
if [ -n "$matches" ]; then
  while IFS= read -r path; do
    echo "::error file=${path}::private key material: ${path}"
  done <<< "$matches"
  found=1
fi

if [ "$found" -ne 0 ]; then
  echo "Sensitive files are committed. Remove them from the repository and, if they were ever pushed, treat what they hold as disclosed."
  exit 1
fi
echo "No sensitive file among $(git ls-files | wc -l | tr -d ' ') committed files."
