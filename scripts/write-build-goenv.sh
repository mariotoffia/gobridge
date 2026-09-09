#!/usr/bin/env bash
# Write native Go build settings without putting the initial document in argv.
set -euo pipefail
umask 077

if [ "$#" -ne 4 ]; then
  echo "usage: write-build-goenv.sh <config-file> <goenv-file> <version> <git-sha>" >&2
  exit 2
fi
source_file=$1
target_file=$2
version=$3
git_sha=$4
if [ ! -f "$source_file" ] || [ "$source_file" -ef "$target_file" ]; then
  echo "build config must be a regular file distinct from the output" >&2
  exit 2
fi
for value in "$version" "$git_sha"; do
  if [[ ! "$value" =~ ^[a-zA-Z0-9_.+@/-]+$ ]]; then
    echo "version and git-sha must contain only build-metadata characters" >&2
    exit 2
  fi
done

previous=$(go env GOENV)
if [ "$previous" = "$target_file" ] || { [ "$previous" != off ] && [ "$previous" -ef "$target_file" ]; }; then
  echo "build output must not overwrite the active Go environment" >&2
  exit 2
fi
: > "$target_file"
chmod 0600 "$target_file"
if [ "$previous" != off ] && [ -f "$previous" ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      GOFLAGS=*) ;;
      *) printf '%s\n' "$line" >> "$target_file" ;;
    esac
  done < "$previous"
fi
{
  printf 'GOFLAGS="-ldflags=-s -w -X main.version=%s -X main.gitSHA=%s -X main.initialConfigBase64=' "$version" "$git_sha"
  base64 < "$source_file" | tr -d '\r\n'
  printf '"\n'
} >> "$target_file"
