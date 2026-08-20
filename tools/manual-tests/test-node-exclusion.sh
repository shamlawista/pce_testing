#!/usr/bin/env bash
# Automated regression test for pola's CSPF node-exclusion feature
# (per-policy exclude by routerID/SID, global node-exclusion set,
# creation-time and reoptimization-time behavior).
#
# Run this directly on the lab machine (needs `pola` and `jq` in PATH,
# and polad already running and reachable).
#
#   chmod +x test-exclusion.sh
#   ./test-exclusion.sh
#
# It will PAUSE and prompt you twice, when a real topology change on the
# router is needed to trigger a reoptimization sweep - everything else is
# fully automatic.

set -uo pipefail

# ---------------------------------------------------------------------------
# Config - edit these to match your lab. Defaults below match the topology
# used during the original CSPF node-exclusion investigation.
# ---------------------------------------------------------------------------
HOST="127.0.0.1"
PORT="50052"
ASN="65018"
SESSION="213.119.192.12"          # pcepSessionAddr for the test policies
SRC_ROUTER="2131.1919.2012"
DST_ROUTER="2131.1919.2021"

# A node that sits on the SRC->DST path and has a known SID - used for the
# per-policy exclude tests.
EXCL_A_ROUTERID="2131.1919.2020"
EXCL_A_SID="20001"

# A second, distinct per-policy-excludable node (used only to prove SID and
# routerID forms are independent, not required to differ topologically).
EXCL_B_ROUTERID="2131.1919.2103"

# The node used for the GLOBAL exclusion set in this test - must be the one
# whose SID actually appears in the unconstrained SRC->DST baseline path.
GLOBAL_ROUTERID="2131.1919.2102"   # IGLABO12
GLOBAL_SID="20013"

# Color range reserved for this script's test policies - pick something far
# from any real production color to avoid collisions.
COLOR_BASE=9100

# Optional: path to polad's log file, if it writes to one (leave empty to
# skip log-based confirmation and rely on `pola sr-policy list` checks only).
LOG_FILE=""

WORKDIR="$(mktemp -d /tmp/pola-exclusion-test.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0
CREATED_POLICIES=()   # "sessionAddr|policyName" pairs, for cleanup

color_red()   { printf '\033[31m%s\033[0m\n' "$1"; }
color_green() { printf '\033[32m%s\033[0m\n' "$1"; }
color_yellow(){ printf '\033[33m%s\033[0m\n' "$1"; }

info()  { echo "  $1"; }
warn()  { color_yellow "  WARN: $1"; }
pass()  { color_green  "PASS: $1"; PASS_COUNT=$((PASS_COUNT+1)); }
fail()  { color_red    "FAIL: $1"; FAIL_COUNT=$((FAIL_COUNT+1)); }

section() {
    echo
    echo "==================================================================="
    echo "$1"
    echo "==================================================================="
}

pola_cli() {
    pola --host "$HOST" --port "$PORT" "$@"
}

# run_pola_add NAME YAML_CONTENT
# Writes YAML_CONTENT to a temp file and runs `pola sr-policy add -f`.
# Sets globals: LAST_OUTPUT, LAST_STATUS
run_pola_add() {
    local name="$1" content="$2" file="$WORKDIR/$name.yaml"
    printf '%s\n' "$content" > "$file"
    LAST_OUTPUT="$(pola_cli sr-policy add -f "$file" 2>&1)"
    LAST_STATUS=$?
}

# get_policy_json SESSION_ADDR POLICY_NAME
# Prints the JSON object for one policy (peerAddr merged in), or empty.
get_policy_json() {
    local session="$1" name="$2"
    pola_cli sr-policy list -j 2>/dev/null \
        | jq -c --arg s "$session" --arg n "$name" \
            '.[] | select(.peerAddr==$s) | . as $sess | $sess.srPolicies[]
             | select(.policyName==$n) | . + {peerAddr: $sess.peerAddr}'
}

# segment_list_contains_sid POLICY_JSON SID
segment_list_contains_sid() {
    echo "$1" | jq -e --argjson sid "$2" '.segmentList[]? | select(.sid == $sid)' >/dev/null 2>&1
}

# delete_policy SESSION_ADDR POLICY_NAME - best-effort cleanup
delete_policy() {
    local session="$1" name="$2"
    local pj file
    pj="$(get_policy_json "$session" "$name")"
    if [ -z "$pj" ]; then
        return 0
    fi
    local dstAddr color
    dstAddr="$(echo "$pj" | jq -r '.dstAddr')"
    color="$(echo "$pj" | jq -r '.color')"
    file="$WORKDIR/delete-$name.yaml"
    cat > "$file" <<EOF
srPolicy:
  pcepSessionAddr: $session
  dstAddr: $dstAddr
  color: $color
  name: $name
EOF
    pola_cli sr-policy delete -f "$file" >/dev/null 2>&1
}

cleanup_all() {
    section "Cleanup"
    for entry in "${CREATED_POLICIES[@]:-}"; do
        [ -z "$entry" ] && continue
        local session="${entry%%|*}" name="${entry##*|}"
        info "deleting $name on $session"
        delete_policy "$session" "$name"
    done
    info "removing any leftover global exclusions this script added"
    pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1 || true
}

log_check() {
    [ -z "$LOG_FILE" ] && return 0
    [ ! -f "$LOG_FILE" ] && return 0
    echo "  --- relevant log lines ---"
    grep -E "$1" "$LOG_FILE" | tail -20 | sed 's/^/  /'
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
section "Preflight"

if ! command -v jq >/dev/null 2>&1; then
    color_red "jq is required but not found in PATH"; exit 1
fi
if ! command -v pola >/dev/null 2>&1; then
    color_red "pola is required but not found in PATH"; exit 1
fi

sessions_json="$(pola_cli session -j 2>&1)" || { color_red "failed to reach polad on $HOST:$PORT"; echo "$sessions_json"; exit 1; }
up_count="$(echo "$sessions_json" | jq '[.[] | select(.State=="SESSION_STATE_UP" and .IsSynced==true)] | length' 2>/dev/null || echo 0)"
info "synced sessions: $up_count"
if [ "$up_count" -lt 1 ]; then
    color_red "no synced PCEP sessions - fix this before running the tests"
    exit 1
fi

existing_global="$(pola_cli node-exclude list -j 2>/dev/null || echo '[]')"
if [ "$(echo "$existing_global" | jq 'length')" != "0" ]; then
    warn "global exclusion set is not empty before starting: $existing_global"
    warn "clearing it so the test run starts from a known state"
    for r in $(echo "$existing_global" | jq -r '.[]'); do
        pola_cli node-exclude remove --routerID "$r" >/dev/null 2>&1
    done
fi
pass "preflight: sessions up, global exclusion set clean"

# ---------------------------------------------------------------------------
# 1. Per-policy exclude by routerID
# ---------------------------------------------------------------------------
section "Test 1: per-policy exclude by routerID"
NAME="test-excl-routerid"
CREATED_POLICIES+=("$SESSION|$NAME")
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+1))
  type: dynamic
  metric: igp
  exclude:
    - routerID: $EXCL_A_ROUTERID
"
if [ $LAST_STATUS -ne 0 ]; then
    fail "creation failed unexpectedly: $LAST_OUTPUT"
else
    pj="$(get_policy_json "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$EXCL_A_SID"; then
        fail "computed path still contains excluded SID $EXCL_A_SID: $pj"
    else
        pass "per-policy exclude by routerID avoids SID $EXCL_A_SID"
    fi
fi

# ---------------------------------------------------------------------------
# 2. Per-policy exclude by SID
# ---------------------------------------------------------------------------
section "Test 2: per-policy exclude by SID (resolves to routerID)"
NAME="test-excl-sid"
CREATED_POLICIES+=("$SESSION|$NAME")
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+2))
  type: dynamic
  metric: igp
  exclude:
    - sid: $EXCL_A_SID
"
if [ $LAST_STATUS -ne 0 ]; then
    fail "creation failed unexpectedly: $LAST_OUTPUT"
else
    pj="$(get_policy_json "$SESSION" "$NAME")"
    resolved="$(echo "$pj" | jq -r '.exclude[0] // empty')"
    if [ "$resolved" != "$EXCL_A_ROUTERID" ]; then
        fail "exclude field shows '$resolved', want resolved routerID $EXCL_A_ROUTERID"
    elif segment_list_contains_sid "$pj" "$EXCL_A_SID"; then
        fail "computed path still contains excluded SID $EXCL_A_SID"
    else
        pass "sid-based exclude resolves to $EXCL_A_ROUTERID and avoids it"
    fi
fi

# ---------------------------------------------------------------------------
# 3. Self-exclusion (source) rejected
# ---------------------------------------------------------------------------
section "Test 3: excluding the policy's own source is rejected"
NAME="test-excl-selfsrc"
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+3))
  type: dynamic
  metric: igp
  exclude:
    - routerID: $SRC_ROUTER
"
if [ $LAST_STATUS -eq 0 ]; then
    fail "expected rejection, but creation succeeded"
    CREATED_POLICIES+=("$SESSION|$NAME")
elif echo "$LAST_OUTPUT" | grep -qi "own source\|no SR-MPLS path\|source"; then
    pass "self-source exclusion rejected: $(echo "$LAST_OUTPUT" | head -1)"
else
    fail "rejected, but not with the expected reason: $LAST_OUTPUT"
fi

# ---------------------------------------------------------------------------
# 4. Self-exclusion (destination) rejected
# ---------------------------------------------------------------------------
section "Test 4: excluding the policy's own destination is rejected"
NAME="test-excl-selfdst"
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+4))
  type: dynamic
  metric: igp
  exclude:
    - routerID: $DST_ROUTER
"
if [ $LAST_STATUS -eq 0 ]; then
    fail "expected rejection, but creation succeeded"
    CREATED_POLICIES+=("$SESSION|$NAME")
elif echo "$LAST_OUTPUT" | grep -qi "own destination\|no SR-MPLS path\|destination"; then
    pass "self-destination exclusion rejected: $(echo "$LAST_OUTPUT" | head -1)"
else
    fail "rejected, but not with the expected reason: $LAST_OUTPUT"
fi

# ---------------------------------------------------------------------------
# 5. Global exclusion set: add/list/remove round trip
# ---------------------------------------------------------------------------
section "Test 5: global exclusion set add/list/remove"
pola_cli node-exclude add --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
listed="$(pola_cli node-exclude list -j 2>/dev/null)"
if echo "$listed" | jq -e --arg r "$GLOBAL_ROUTERID" 'index($r) != null' >/dev/null 2>&1; then
    pass "node-exclude add + list shows $GLOBAL_ROUTERID"
else
    fail "node-exclude list does not show $GLOBAL_ROUTERID after add: $listed"
fi
pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
listed="$(pola_cli node-exclude list -j 2>/dev/null)"
if [ "$(echo "$listed" | jq 'length')" = "0" ]; then
    pass "node-exclude remove empties the list"
else
    fail "list not empty after remove: $listed"
fi

# ---------------------------------------------------------------------------
# 6. Global exclusion applied at creation time (no per-policy exclude)
# ---------------------------------------------------------------------------
section "Test 6: global exclusion applied to a fresh policy with no per-policy exclude"
pola_cli node-exclude add --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
NAME="test-excl-globalonly"
CREATED_POLICIES+=("$SESSION|$NAME")
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+6))
  type: dynamic
  metric: igp
"
if [ $LAST_STATUS -ne 0 ]; then
    fail "creation failed unexpectedly: $LAST_OUTPUT"
else
    pj="$(get_policy_json "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        fail "path still contains globally-excluded SID $GLOBAL_SID"
    else
        pass "global exclusion alone reroutes a fresh policy away from SID $GLOBAL_SID"
    fi
fi

# ---------------------------------------------------------------------------
# 7. Combined per-policy + global exclude at creation -> confirmed no-path
# ---------------------------------------------------------------------------
section "Test 7: per-policy + global exclude combined leaves no path (confirmed topology fact)"
NAME="test-excl-combined-nopath"
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+7))
  type: dynamic
  metric: igp
  exclude:
    - routerID: $EXCL_A_ROUTERID
"
if [ $LAST_STATUS -eq 0 ]; then
    fail "expected 'no path found' with both exclusions active, but creation succeeded"
    CREATED_POLICIES+=("$SESSION|$NAME")
elif echo "$LAST_OUTPUT" | grep -qi "no SR-MPLS path found\|no path"; then
    pass "combined exclusion correctly yields a clean no-path error"
else
    fail "creation failed, but not with the expected 'no path' reason: $LAST_OUTPUT"
fi
pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1

# ---------------------------------------------------------------------------
# 8. Reoptimization: live reroute + revert (needs a manual topology trigger)
# ---------------------------------------------------------------------------
section "Test 8: reoptimization applies/reverts the global exclusion live"

NAME="test-reopt-demo"
CREATED_POLICIES+=("$SESSION|$NAME")
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+8))
  type: dynamic
  metric: igp
"
if [ $LAST_STATUS -ne 0 ]; then
    fail "baseline policy creation failed, skipping reoptimization test: $LAST_OUTPUT"
else
    pj="$(get_policy_json "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        info "baseline path uses SID $GLOBAL_SID, as expected"
    else
        warn "baseline path does not use SID $GLOBAL_SID - this test's assumptions may not hold for your topology"
    fi

    pola_cli node-exclude add --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
    echo
    color_yellow ">>> ACTION NEEDED <<<"
    echo "  Global exclusion of $GLOBAL_ROUTERID is now active."
    echo "  Please trigger a real topology change on the router now"
    echo "  (e.g. shut/no-shut an unrelated IS-IS interface), so a BGP-LS"
    echo "  TED update fires and polad's reoptimization sweep runs."
    read -r -p "  Press Enter once you've done that... "
    sleep 5

    pj="$(get_policy_json "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        fail "path still uses SID $GLOBAL_SID after adding the global exclusion + reoptimizing"
    else
        pass "reoptimization correctly rerouted $NAME away from SID $GLOBAL_SID"
    fi
    log_check "reoptimizing dynamic policy.*$NAME|reoptimization sweep complete"

    pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
    echo
    color_yellow ">>> ACTION NEEDED <<<"
    echo "  Global exclusion removed. Trigger another topology change now"
    echo "  (shut/no-shut again) so reoptimization can revert the path."
    read -r -p "  Press Enter once you've done that... "
    sleep 5

    pj="$(get_policy_json "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        pass "reoptimization correctly reverted $NAME back to SID $GLOBAL_SID after removing the global exclusion"
    else
        warn "path did not revert to SID $GLOBAL_SID - it may simply be a more expensive alternative now; check manually if unexpected"
    fi
    log_check "reoptimizing dynamic policy.*$NAME|reoptimization sweep complete"
fi

# ---------------------------------------------------------------------------
# Summary + cleanup
# ---------------------------------------------------------------------------
cleanup_all

section "Summary"
echo "  Passed: $PASS_COUNT"
echo "  Failed: $FAIL_COUNT"
if [ "$FAIL_COUNT" -eq 0 ]; then
    color_green "ALL TESTS PASSED"
    exit 0
else
    color_red "SOME TESTS FAILED"
    exit 1
fi
