#!/bin/sh
# Review class of a branch (#460). Usage: review-class.sh <branch> [base=origin/main]
# Diffs base...branch, derives the class with gate-class.sh (the gate's one
# derivation) and prints the class and the required review/* context(s) as
# gate-check.sh maps them: carve-out needs review/opus or
# review/gpt-6.1-sol-high; ordinary also accepts review/sonnet and
# review/gpt-6.1-sol-medium. The mapping is mirrored by hand, not shared.
set -u
if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: review-class.sh <branch> [base=origin/main]" >&2
  exit 2
fi
dir=$(dirname "$0")
branch=$1
base=${2:-origin/main}
# NUL separators cannot live in a shell variable: pipe straight into gate-class.sh.
# A failing git diff must not read as an empty, ordinary change.
tmp=$(mktemp) || exit 2
trap 'rm -f "$tmp"' EXIT
if ! git diff --name-only --no-renames -z --end-of-options "$base...$branch" >"$tmp"; then
  echo "review-class: git diff failed" >&2
  exit 2
fi
class=$(sh "$dir/gate-class.sh" <"$tmp")
case "$class" in
ordinary) need="review/sonnet or review/opus or review/gpt-6.1-sol-medium or review/gpt-6.1-sol-high" ;;
carve-out) need="review/opus or review/gpt-6.1-sol-high" ;;
*)
  echo "review-class: unexpected class" >&2
  exit 2
  ;;
esac
echo "class: $class"
echo "required: $need"
echo "(mapping mirrors gate-check.sh)"
