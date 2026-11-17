#!/usr/bin/env bash
# Fails if total statement coverage in a Go coverage profile is below a
# threshold. Usage: check-coverage.sh coverage.out 65
set -euo pipefail
profile="$1"
min="$2"
total="$(go tool cover -func="$profile" | awk '/^total:/ {sub("%", "", $3); print $3}')"
echo "Total coverage: ${total}% (minimum ${min}%)"
awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 >= m + 0) }' || {
  echo "Coverage is below the minimum." >&2
  exit 1
}
