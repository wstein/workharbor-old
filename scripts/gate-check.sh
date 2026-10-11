#!/bin/sh
# Review gate decision (#411, design pr-flow-landing "Flow" and "Security consequences").
# Usage: gate-check.sh <ordinary|carve-out> <statuses.json> <login>...
# <statuses.json> is the output of GET /repos/{r}/commits/{sha}/statuses: one array,
# or several (gh api --paginate --slurp). The latest status per context decides
# (highest id), so a newer failure supersedes an older success. Passes only when a
# required tier is the latest for its context, has state success and was created
# by one of the allowed logins. carve-out needs review/opus or
# review/gpt-6.1-sol-high; ordinary also accepts review/sonnet and
# review/gpt-6.1-sol-medium. Empty login arguments are ignored; no login at all fails.
# Everything read from the JSON is data and is only compared by jq, never evaluated.
set -u
if [ "$#" -lt 3 ]; then
  echo "usage: gate-check.sh <ordinary|carve-out> <statuses.json> <login>..." >&2
  exit 2
fi
class=$1
file=$2
shift 2
case "$class" in
ordinary) tiers='["review/sonnet","review/opus","review/gpt-6.1-sol-medium","review/gpt-6.1-sol-high"]' ;;
carve-out) tiers='["review/opus","review/gpt-6.1-sol-high"]' ;;
*)
  echo "gate: unknown class" >&2
  exit 2
  ;;
esac
logins=$(printf '%s\n' "$@" | jq -R . | jq -s 'map(select(length > 0))')
if ! jq -e 'type == "array"' "$file" >/dev/null 2>&1; then
  echo "gate: statuses file is not a JSON array" >&2
  exit 2
fi
if jq -e --argjson tiers "$tiers" --argjson logins "$logins" '
  flatten
  | group_by(.context)
  | map(max_by(.id))
  | any(.[]; (.context as $c | $tiers | index($c) != null)
      and .state == "success"
      and (.creator.login as $l | $logins | index($l) != null))
' "$file" >/dev/null; then
  echo "gate: pass ($class)"
else
  echo "gate: fail, $class needs a success status from an allowed account for: $(printf '%s' "$tiers" | jq -r 'join(" or ")')" >&2
  exit 1
fi
