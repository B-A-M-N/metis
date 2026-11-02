#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 || ! $1 =~ ^[a-z0-9-]+$ ]]; then
  echo "usage: bash scripts/metis-worktree.sh <role> [base]" >&2
  exit 2
fi

role=$1
base=${2:-HEAD}
root=$(git rev-parse --show-toplevel)
branch="agent/metis-${role}"
worktree="/tmp/metis-${role}"
state="/tmp/metis-state-${role}"

if [[ -e $worktree ]]; then
  echo "worktree path already exists: $worktree" >&2
  exit 1
fi
if git -C "$root" show-ref --verify --quiet "refs/heads/$branch"; then
  echo "branch already exists: $branch" >&2
  exit 1
fi

ref_path=$(git -C "$root" rev-parse --path-format=absolute --git-path "refs/heads/$branch")
mkdir -p "$(dirname "$ref_path")"
git -C "$root" worktree add -b "$branch" "$worktree" "$base"
mkdir -m 700 -p "$state"
printf 'worktree=%s\nbranch=%s\nMETIS_STATE=%s\n' "$worktree" "$branch" "$state"
