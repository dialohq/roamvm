#!/usr/bin/env bash
set -euo pipefail
cmp "$SCENARIO_DATA/response" "$SCENARIO_DATA/payload-$1"
