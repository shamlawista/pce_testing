#!/usr/bin/env bash
# Comprehensive regression test for pola/polad: sessions, TED, explicit and
# dynamic SR policies (all metrics, waypoints, --no-sid-validate, endpoint-
# address form), per-policy and global node exclusion, native reoptimization,
# intent persistence across a polad restart, session delete/reconnect, and
# the sr-mesh full-mesh provisioner's idempotency.
#
# Run this directly on the lab machine (needs `pola`, `jq`, `python3` (or
# `python`) in PATH, and polad already running and reachable).
#
#   chmod +x test-full.sh
#   ./test-full.sh
#
# Two sections pause for you:
#   - Reoptimization (needs a real topology change - shut/no-shut an
#     interface - to trigger a BGP-LS/TED update).
#   - Session delete (asks y/N before closing a live PCEP session).
# Everything else, including restarting polad itself, is fully automatic.

set -uo pipefail

# ---------------------------------------------------------------------------
# Config - edit these to match your lab.
# ---------------------------------------------------------------------------
HOST="127.0.0.1"
PORT="50052"
ASN="65018"
SESSION="213.119.192.12"          # default pcepSessionAddr, used as a fallback

POLAD_BIN="$HOME/go/bin/polad"
POLAD_CONFIG="$HOME/pola-run/polad.yaml"
POLAD_RESTART_LOG="$HOME/pola-run/polad-test-restart.log"

MESH_SCRIPT="$HOME/pce_testing/tools/sr-mesh/sr_mesh_provision.py"

# Node-exclusion topology facts confirmed during the original investigation.
SRC_ROUTER="2131.1919.2012"
DST_ROUTER="2131.1919.2021"
EXCL_A_ROUTERID="2131.1919.2020"
EXCL_A_SID="20001"
GLOBAL_ROUTERID="2131.1919.2102"   # IGLABO12
GLOBAL_SID="20013"

COLOR_BASE=9100

WORKDIR="$(mktemp -d /tmp/pola-full-test.XXXXXX)"
trap 'rm -rf "$WORKDIR"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0
CREATED_POLICIES=()   # "sessionAddr|policyName" pairs, for cleanup

color_red()   { printf '\033[31m%s\033[0m\n' "$1"; }
color_green() { printf '\033[32m%s\033[0m\n' "$1"; }
color_yellow(){ printf '\033[33m%s\033[0m\n' "$1"; }

info()  { echo "  $1"; }
warn()  { color_yellow "  WARN: $1"; WARN_COUNT=$((WARN_COUNT+1)); }
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

python_bin() {
    if command -v python3 >/dev/null 2>&1; then echo python3; else echo python; fi
}

# run_pola_add NAME YAML_CONTENT
# Sets globals: LAST_OUTPUT, LAST_STATUS
run_pola_add() {
    local name="$1"
    local content="$2"
    local file="$WORKDIR/$name.yaml"
    printf '%s\n' "$content" > "$file"
    LAST_OUTPUT="$(pola_cli sr-policy add -f "$file" 2>&1)"
    LAST_STATUS=$?
}

get_policy_json() {
    local session="$1" name="$2"
    pola_cli sr-policy list -j 2>/dev/null \
        | jq -c --arg s "$session" --arg n "$name" \
            '.[] | select(.peerAddr==$s) | . as $sess | $sess.srPolicies[]
             | select(.policyName==$n) | . + {peerAddr: $sess.peerAddr}'
}

# wait_for_policy SESSION NAME [TIMEOUT] - polls until the policy appears
# with a non-empty segmentList. CreateSRPolicy/SendPCUpdate return as soon
# as the PCInitiate/PCUpdate is SENT, not once the confirming PCRpt has
# been processed and the policy is visible via `sr-policy list` - reading
# immediately after `sr-policy add` is a race, not a real bug.
wait_for_policy() {
    local session="$1" name="$2" timeout="${3:-20}" waited=0
    local pj
    while [ "$waited" -lt "$timeout" ]; do
        pj="$(get_policy_json "$session" "$name")"
        if [ -n "$pj" ] && [ "$(echo "$pj" | jq '.segmentList | length' 2>/dev/null || echo 0)" -gt 0 ]; then
            echo "$pj"
            return 0
        fi
        sleep 1
        waited=$((waited+1))
    done
    get_policy_json "$session" "$name"
    return 1
}

segment_list_contains_sid() {
    echo "$1" | jq -e --argjson sid "$2" '.segmentList[]? | select(.sid == $sid)' >/dev/null 2>&1
}

segment_list_sids() {
    echo "$1" | jq -c '[.segmentList[]?.sid]'
}

delete_policy() {
    local session="$1" name="$2"
    local pj file dstAddr color
    pj="$(get_policy_json "$session" "$name")"
    [ -z "$pj" ] && return 0
    dstAddr="$(echo "$pj" | jq -r '.dstAddr')"
    color="$(echo "$pj" | jq -r '.color')"
    [ "$dstAddr" = "null" ] || [ -z "$dstAddr" ] && return 0
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

resolve_sid() {
    local router="$1"
    pola_cli ted -j 2>/dev/null | jq -r --arg r "$router" \
        '.ted[]? | select(.routerID==$r) | (.srgbBegin + ([.prefixes[]? | select(.sidIndex != null) | .sidIndex][0])) // empty'
}

resolve_loopback() {
    local router="$1"
    pola_cli ted -j 2>/dev/null | jq -r --arg r "$router" \
        '.ted[]? | select(.routerID==$r) | .prefixes[]? | select(.sidIndex != null) | .prefix' \
        | head -1 | cut -d/ -f1
}

wait_for_synced_session() {
    local timeout="${1:-60}" waited=0
    while [ "$waited" -lt "$timeout" ]; do
        local n
        n="$(pola_cli session -j 2>/dev/null | jq '[.[] | select(.IsSynced==true)] | length' 2>/dev/null || echo 0)"
        if [ "${n:-0}" -ge 1 ]; then
            return 0
        fi
        sleep 2
        waited=$((waited+2))
    done
    return 1
}

# settle_policy_count polls the total SR policy count until it stops
# changing across a 3s interval (up to 30s), so a just-finished mesh
# provisioner run's still-in-flight PCInitiates don't get miscounted as
# "extra" policies appearing on the NEXT run.
settle_policy_count() {
    local prev="" cur waited=0
    cur="$(pola_cli sr-policy list -j 2>/dev/null | jq '[.[].srPolicies[]] | length')"
    while [ "$waited" -lt 30 ]; do
        sleep 3
        waited=$((waited+3))
        prev="$cur"
        cur="$(pola_cli sr-policy list -j 2>/dev/null | jq '[.[].srPolicies[]] | length')"
        if [ "$prev" = "$cur" ]; then
            echo "$cur"
            return 0
        fi
    done
    echo "$cur"
}

restart_polad() {
    if [ ! -x "$POLAD_BIN" ]; then
        warn "POLAD_BIN ($POLAD_BIN) not found/executable - cannot restart automatically"
        return 1
    fi
    local pid
    pid="$(pgrep -x polad | head -1)"
    if [ -z "$pid" ]; then
        warn "no running polad process found (pgrep -x polad) - cannot test restart persistence automatically"
        return 1
    fi
    info "killing polad (PID $pid)"
    kill "$pid" 2>/dev/null
    for _ in $(seq 1 20); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 1
    done
    if kill -0 "$pid" 2>/dev/null; then
        warn "polad (PID $pid) did not exit after SIGTERM, sending SIGKILL"
        kill -9 "$pid" 2>/dev/null
        sleep 1
    fi
    info "restarting polad ($POLAD_BIN -f $POLAD_CONFIG), logging to $POLAD_RESTART_LOG"
    nohup "$POLAD_BIN" -f "$POLAD_CONFIG" > "$POLAD_RESTART_LOG" 2>&1 &
    disown
    for _ in $(seq 1 30); do
        if pola_cli session -j >/dev/null 2>&1; then
            info "polad's gRPC API is back up"
            if wait_for_synced_session 60; then
                info "at least one PCEP session resynced"
                return 0
            else
                warn "gRPC is up but no session resynced within 60s"
                return 1
            fi
        fi
        sleep 1
    done
    fail "polad did not come back up within 30s after restart"
    return 1
}

# ---------------------------------------------------------------------------
# Preflight + auto-discovery
# ---------------------------------------------------------------------------
section "Preflight"

for bin in jq pola; do
    if ! command -v "$bin" >/dev/null 2>&1; then
        color_red "$bin is required but not found in PATH"; exit 1
    fi
done
PY="$(python_bin)"
if ! command -v "$PY" >/dev/null 2>&1; then
    warn "no python3/python found - the sr-mesh section will be skipped"
    PY=""
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
    warn "global exclusion set is not empty before starting: $existing_global - clearing it"
    for r in $(echo "$existing_global" | jq -r '.[]'); do
        pola_cli node-exclude remove --routerID "$r" >/dev/null 2>&1
    done
fi

GENERIC_OK=0
sr_capable="$(pola_cli ted -j 2>/dev/null | jq -r '
    .ted[]? | select((.srgbBegin // 0) > 0)
    | select([.prefixes[]? | select(.sidIndex != null)] | length > 0)
    | .routerID')"
GENERIC_SRC="$(echo "$sr_capable" | sed -n '1p')"
GENERIC_DST="$(echo "$sr_capable" | sed -n '2p')"
GENERIC_WAYPOINT="$(echo "$sr_capable" | sed -n '3p')"
if [ -n "$GENERIC_SRC" ] && [ -n "$GENERIC_DST" ] && [ -n "$GENERIC_WAYPOINT" ]; then
    GENERIC_OK=1
    info "auto-discovered generic test nodes: SRC=$GENERIC_SRC DST=$GENERIC_DST WAYPOINT=$GENERIC_WAYPOINT"
else
    warn "could not auto-discover 3 distinct SR-capable TED nodes - explicit/dynamic/waypoint generic tests will be skipped"
fi

GENERIC_SESSION="$(echo "$sessions_json" | jq -r '[.[] | select(.State=="SESSION_STATE_UP" and .IsSynced==true)][0].Addr // empty')"
[ -z "$GENERIC_SESSION" ] && GENERIC_SESSION="$SESSION"
info "using session $GENERIC_SESSION for generic-path tests"

info "clearing any leftover test policies from a previous run"
for n in test-scratch-dynamic test-explicit-routerid test-explicit-badsid test-explicit-endpointaddr          test-dynamic-metric-igp test-dynamic-metric-te test-dynamic-metric-delay test-waypoints          test-excl-routerid test-excl-sid test-excl-selfsrc test-excl-selfdst test-excl-globalonly          test-excl-combined-nopath test-reopt-demo test-persist-restart; do
    delete_policy "$SESSION" "$n"
    delete_policy "$GENERIC_SESSION" "$n"
done

pass "preflight complete"

# ---------------------------------------------------------------------------
# 1. Session & TED reads
# ---------------------------------------------------------------------------
section "Test 1: session and TED read APIs"

if echo "$sessions_json" | jq -e '.[0].Capabilities' >/dev/null 2>&1; then
    pass "pola session -j returns capability details"
else
    fail "pola session -j output missing expected Capabilities field"
fi

ted_json="$(pola_cli ted -j 2>&1)"
node_count="$(echo "$ted_json" | jq '.ted | length' 2>/dev/null || echo 0)"
if [ "${node_count:-0}" -gt 0 ]; then
    pass "pola ted -j returns $node_count nodes"
else
    fail "pola ted -j returned no nodes: $ted_json"
fi

# ---------------------------------------------------------------------------
# 2. Explicit path - router-ID form, using real SIDs from a scratch dynamic
#    computation (proves explicit provisioning end-to-end with valid SIDs)
# ---------------------------------------------------------------------------
section "Test 2: explicit path (router-ID form)"

if [ "$GENERIC_OK" = "1" ]; then
    SCRATCH="test-scratch-dynamic"
    run_pola_add "$SCRATCH" "asn: $ASN
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  name: $SCRATCH
  srcRouterID: $GENERIC_SRC
  dstRouterID: $GENERIC_DST
  color: $((COLOR_BASE+20))
  type: dynamic
  metric: igp
"
    if [ $LAST_STATUS -ne 0 ]; then
        fail "scratch dynamic policy (for real SIDs) failed: $LAST_OUTPUT"
    else
        pj="$(wait_for_policy "$GENERIC_SESSION" "$SCRATCH")"
        sids="$(echo "$pj" | jq -r '.segmentList[]?.sid' | tr '\n' ' ')"
        delete_policy "$GENERIC_SESSION" "$SCRATCH"

        if [ -z "$sids" ]; then
            fail "scratch dynamic policy returned no SIDs to reuse"
        else
            NAME="test-explicit-routerid"
            CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
            seglist=""
            for s in $sids; do seglist="$seglist    - sid: $s
"; done
            run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  name: $NAME
  srcRouterID: $GENERIC_SRC
  dstRouterID: $GENERIC_DST
  color: $((COLOR_BASE+21))
  type: explicit
  segmentList:
$seglist"
            if [ $LAST_STATUS -ne 0 ]; then
                fail "explicit router-ID form creation failed: $LAST_OUTPUT"
            else
                pj="$(wait_for_policy "$GENERIC_SESSION" "$NAME")"
                got="$(echo "$pj" | jq -c '[.segmentList[]?.sid]')"
                want="$(printf '%s\n' $sids | jq -R . | jq -cs 'map(tonumber)')"
                if [ "$got" = "$want" ]; then
                    pass "explicit router-ID form round-trips the exact segment list ($got)"
                else
                    fail "explicit segment list $got does not match requested $want"
                fi
            fi
        fi
    fi
else
    warn "skipping explicit-path test (no generic nodes discovered)"
fi

# ---------------------------------------------------------------------------
# 3. Explicit path - --no-sid-validate
# ---------------------------------------------------------------------------
section "Test 3: --no-sid-validate"

if [ "$GENERIC_OK" = "1" ]; then
    NAME="test-explicit-badsid"
    run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  name: $NAME
  srcRouterID: $GENERIC_SRC
  dstRouterID: $GENERIC_DST
  color: $((COLOR_BASE+22))
  type: explicit
  segmentList:
    - sid: 999999
"
    if [ $LAST_STATUS -eq 0 ]; then
        fail "explicit policy with a nonexistent SID succeeded without --no-sid-validate"
        CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
    elif echo "$LAST_OUTPUT" | grep -qi "no-sid-validate\|not found\|does not exist\|missing from"; then
        pass "bogus SID correctly rejected without --no-sid-validate"
    else
        fail "rejected, but not with the expected SID-validation reason: $LAST_OUTPUT"
    fi

    file="$WORKDIR/$NAME.yaml"
    LAST_OUTPUT="$(pola_cli sr-policy add -f "$file" --no-sid-validate 2>&1)"
    LAST_STATUS=$?
    if [ $LAST_STATUS -eq 0 ]; then
        pass "same request succeeds with --no-sid-validate"
        CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
    else
        fail "request still failed with --no-sid-validate: $LAST_OUTPUT"
    fi
else
    warn "skipping --no-sid-validate test (no generic nodes discovered)"
fi

# ---------------------------------------------------------------------------
# 4. Explicit path - endpoint-address form
# ---------------------------------------------------------------------------
section "Test 4: explicit path (endpoint-address form)"

if [ "$GENERIC_OK" = "1" ]; then
    src_addr="$(resolve_loopback "$GENERIC_SRC")"
    dst_addr="$(resolve_loopback "$GENERIC_DST")"
    src_sid="$(resolve_sid "$GENERIC_SRC")"
    dst_sid="$(resolve_sid "$GENERIC_DST")"
    if [ -z "$src_addr" ] || [ -z "$dst_addr" ] || [ -z "$src_sid" ] || [ -z "$dst_sid" ]; then
        warn "could not resolve loopback/SID for generic nodes - skipping endpoint-address test"
    else
        NAME="test-explicit-endpointaddr"
        file="$WORKDIR/$NAME.yaml"
        cat > "$file" <<EOF
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  srcAddr: "$src_addr"
  dstAddr: "$dst_addr"
  name: $NAME
  color: $((COLOR_BASE+23))
  segmentList:
    - sid: $dst_sid
EOF
        LAST_OUTPUT="$(pola_cli sr-policy add -f "$file" 2>&1)"
        LAST_STATUS=$?
        if [ $LAST_STATUS -ne 0 ]; then
            fail "endpoint-address form creation failed: $LAST_OUTPUT"
        else
            CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
            pass "endpoint-address form (srcAddr/dstAddr) creates successfully"
        fi
    fi
else
    warn "skipping endpoint-address test (no generic nodes discovered)"
fi

# ---------------------------------------------------------------------------
# 5. Dynamic path - all metrics
# ---------------------------------------------------------------------------
section "Test 5: dynamic path metrics (igp / te / delay)"

if [ "$GENERIC_OK" = "1" ]; then
    i=0
    for metric in igp te delay; do
        i=$((i+1))
        NAME="test-dynamic-metric-$metric"
        CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
        run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  name: $NAME
  srcRouterID: $GENERIC_SRC
  dstRouterID: $GENERIC_DST
  color: $((COLOR_BASE+30+i))
  type: dynamic
  metric: $metric
"
        if [ $LAST_STATUS -ne 0 ]; then
            if echo "$LAST_OUTPUT" | grep -qi "metric.*not defined"; then
                warn "metric=$metric is not present on this topology's links (check IS-IS TE/delay advertisement if unexpected - not a polad bug)"
            else
                fail "metric=$metric creation failed: $LAST_OUTPUT"
            fi
        else
            pj="$(wait_for_policy "$GENERIC_SESSION" "$NAME")"
            seglen="$(echo "$pj" | jq '.segmentList | length' 2>/dev/null)"
            seglen="${seglen:-0}"
            if [ "$seglen" -gt 0 ]; then
                pass "metric=$metric computes a non-empty path"
            else
                fail "metric=$metric returned an empty segment list"
            fi
        fi
    done
else
    warn "skipping dynamic-metric tests (no generic nodes discovered)"
fi

# ---------------------------------------------------------------------------
# 6. Dynamic path - loose source routing (waypoints)
# ---------------------------------------------------------------------------
section "Test 6: loose source routing (waypoints)"

if [ "$GENERIC_OK" = "1" ]; then
    wp_sid="$(resolve_sid "$GENERIC_WAYPOINT")"
    NAME="test-waypoints"
    CREATED_POLICIES+=("$GENERIC_SESSION|$NAME")
    run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $GENERIC_SESSION
  name: $NAME
  srcRouterID: $GENERIC_SRC
  dstRouterID: $GENERIC_DST
  color: $((COLOR_BASE+40))
  type: dynamic
  metric: igp
  waypoints:
    - routerID: $GENERIC_WAYPOINT
"
    if [ $LAST_STATUS -ne 0 ]; then
        fail "waypoint policy creation failed: $LAST_OUTPUT"
    else
        pj="$(wait_for_policy "$GENERIC_SESSION" "$NAME")"
        if [ -n "$wp_sid" ] && segment_list_contains_sid "$pj" "$wp_sid"; then
            pass "computed path transits the waypoint's SID ($wp_sid)"
        else
            fail "computed path does not contain the waypoint's SID ($wp_sid): $(segment_list_sids "$pj")"
            info "full policy JSON for diagnosis: $pj"
            info "waypoint router $GENERIC_WAYPOINT resolved SID: $wp_sid"
        fi
    fi
else
    warn "skipping waypoint test (no generic nodes discovered)"
fi

# ---------------------------------------------------------------------------
# 7. Node exclusion - per-policy (routerID / SID / self-src / self-dst)
# ---------------------------------------------------------------------------
section "Test 7: per-policy node exclusion"

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
    pj="$(wait_for_policy "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$EXCL_A_SID"; then
        fail "computed path still contains excluded SID $EXCL_A_SID"
    else
        pass "per-policy exclude by routerID avoids SID $EXCL_A_SID"
    fi
fi

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
    pj="$(wait_for_policy "$SESSION" "$NAME")"
    resolved="$(echo "$pj" | jq -r '.exclude[0] // empty')"
    if [ "$resolved" != "$EXCL_A_ROUTERID" ]; then
        fail "exclude field shows '$resolved', want resolved routerID $EXCL_A_ROUTERID"
    elif segment_list_contains_sid "$pj" "$EXCL_A_SID"; then
        fail "computed path still contains excluded SID $EXCL_A_SID"
    else
        pass "sid-based exclude resolves to $EXCL_A_ROUTERID and avoids it"
    fi
fi

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
    pass "self-source exclusion rejected"
else
    fail "rejected, but not with the expected reason: $LAST_OUTPUT"
fi

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
    pass "self-destination exclusion rejected"
else
    fail "rejected, but not with the expected reason: $LAST_OUTPUT"
fi

# ---------------------------------------------------------------------------
# 8. Node exclusion - global set (add/list/remove, creation-time, combined)
# ---------------------------------------------------------------------------
section "Test 8: global node-exclusion set"

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
    pj="$(wait_for_policy "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        fail "path still contains globally-excluded SID $GLOBAL_SID"
    else
        pass "global exclusion alone reroutes a fresh policy away from SID $GLOBAL_SID"
    fi
fi

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
    pass "combined per-policy + global exclusion correctly yields a clean no-path error"
else
    fail "creation failed, but not with the expected 'no path' reason: $LAST_OUTPUT"
fi
pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1

# ---------------------------------------------------------------------------
# 9. Reoptimization: live reroute + revert (needs a manual topology trigger)
# ---------------------------------------------------------------------------
section "Test 9: reoptimization applies/reverts the global exclusion live"

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
    pj="$(wait_for_policy "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        info "baseline path uses SID $GLOBAL_SID, as expected"
    else
        warn "baseline path does not use SID $GLOBAL_SID - this test's assumptions may not hold for your topology"
    fi

    pola_cli node-exclude add --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
    echo
    color_yellow ">>> ACTION NEEDED <<<"
    echo "  Global exclusion of $GLOBAL_ROUTERID is now active."
    echo "  Trigger a real topology change on the router now (e.g. shut/"
    echo "  no-shut an unrelated IS-IS interface) so a BGP-LS TED update"
    echo "  fires and polad's reoptimization sweep runs."
    read -r -p "  Press Enter once you've done that... "
    sleep 5

    pj="$(wait_for_policy "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        fail "path still uses SID $GLOBAL_SID after adding the global exclusion + reoptimizing"
    else
        pass "reoptimization correctly rerouted $NAME away from SID $GLOBAL_SID"
    fi

    pola_cli node-exclude remove --routerID "$GLOBAL_ROUTERID" >/dev/null 2>&1
    echo
    color_yellow ">>> ACTION NEEDED <<<"
    echo "  Global exclusion removed. Trigger another topology change now"
    echo "  (shut/no-shut again) so reoptimization can revert the path."
    read -r -p "  Press Enter once you've done that... "
    sleep 5

    pj="$(wait_for_policy "$SESSION" "$NAME")"
    if segment_list_contains_sid "$pj" "$GLOBAL_SID"; then
        pass "reoptimization correctly reverted $NAME back to SID $GLOBAL_SID"
    else
        warn "path did not revert to SID $GLOBAL_SID - may just be a costlier alternative now; check manually if unexpected"
    fi
fi

# ---------------------------------------------------------------------------
# 10. Intent persistence across a polad restart (automated kill + restart)
# ---------------------------------------------------------------------------
section "Test 10: intent persistence survives a polad restart"

NAME="test-persist-restart"
CREATED_POLICIES+=("$SESSION|$NAME")
run_pola_add "$NAME" "asn: $ASN
srPolicy:
  pcepSessionAddr: $SESSION
  name: $NAME
  srcRouterID: $SRC_ROUTER
  dstRouterID: $DST_ROUTER
  color: $((COLOR_BASE+50))
  type: dynamic
  metric: igp
  exclude:
    - routerID: $EXCL_A_ROUTERID
"
if [ $LAST_STATUS -ne 0 ]; then
    fail "baseline policy creation failed, skipping persistence test: $LAST_OUTPUT"
else
    before="$(wait_for_policy "$SESSION" "$NAME")"
    before_type="$(echo "$before" | jq -r '.type')"
    before_metric="$(echo "$before" | jq -r '.metric')"
    before_exclude="$(echo "$before" | jq -c '.exclude')"

    if restart_polad; then
        after="$(wait_for_policy "$SESSION" "$NAME")"
        if [ -z "$after" ]; then
            fail "policy $NAME is gone after restart"
        else
            after_type="$(echo "$after" | jq -r '.type')"
            after_metric="$(echo "$after" | jq -r '.metric')"
            after_exclude="$(echo "$after" | jq -c '.exclude')"
            if [ "$before_type" = "$after_type" ] && [ "$before_metric" = "$after_metric" ] && [ "$before_exclude" = "$after_exclude" ]; then
                pass "type/metric/exclude survived the restart unchanged (type=$after_type metric=$after_metric exclude=$after_exclude)"
            else
                fail "intent changed across restart: before(type=$before_type metric=$before_metric exclude=$before_exclude) after(type=$after_type metric=$after_metric exclude=$after_exclude)"
            fi
        fi
    else
        warn "could not complete automated restart - persistence not verified this run"
    fi
fi

# ---------------------------------------------------------------------------
# 11. Session delete + reconnect (confirm before doing anything disruptive)
# ---------------------------------------------------------------------------
section "Test 11: session delete + reconnect (DISRUPTIVE)"

echo
color_yellow ">>> CONFIRMATION NEEDED <<<"
echo "  About to run: pola session delete $SESSION"
echo "  This closes a live PCEP session - every dynamic policy on it stops"
echo "  being reoptimized until the PCC reconnects."
read -r -p "  Continue? [y/N] " confirm
if [[ "$confirm" =~ ^[Yy]$ ]]; then
    pola_cli session delete "$SESSION" >/dev/null 2>&1
    info "waiting up to 90s for $SESSION to reconnect and resync..."
    reconnected=0
    for _ in $(seq 1 45); do
        state="$(pola_cli session -j 2>/dev/null | jq -r --arg s "$SESSION" '.[] | select(.Addr==$s) | select(.State=="SESSION_STATE_UP" and .IsSynced==true) | .Addr // empty')"
        if [ "$state" = "$SESSION" ]; then
            reconnected=1
            break
        fi
        sleep 2
    done
    if [ "$reconnected" = "1" ]; then
        pass "session $SESSION reconnected and resynced after delete"
    else
        warn "session $SESSION did not reconnect within 90s - reconnect behavior is router-config-dependent, check manually"
    fi
else
    warn "session-delete test skipped (not confirmed)"
fi

# ---------------------------------------------------------------------------
# 12. sr-mesh full-mesh provisioner idempotency
# ---------------------------------------------------------------------------
section "Test 12: sr-mesh provisioner idempotency"

if [ -z "$PY" ]; then
    warn "no python interpreter found, skipping sr-mesh test"
elif [ ! -f "$MESH_SCRIPT" ]; then
    warn "sr_mesh_provision.py not found at $MESH_SCRIPT, skipping"
else
    wait_for_synced_session 30 || true
    info "running sr-mesh provisioner (run 1/2) - this may create new dynamic policies"
    out1="$($PY "$MESH_SCRIPT" --host "$HOST" --port "$PORT" --asn "$ASN" -v 2>&1)"
    status1=$?
    count_before="$(settle_policy_count)"
    info "policy count after run 1 (settled): $count_before"

    info "running sr-mesh provisioner (run 2/2) - should be a no-op"
    out2="$($PY "$MESH_SCRIPT" --host "$HOST" --port "$PORT" --asn "$ASN" -v 2>&1)"
    status2=$?
    count_after="$(settle_policy_count)"
    info "policy count after run 2 (settled): $count_after"

    if [ $status1 -ne 0 ] || [ $status2 -ne 0 ]; then
        fail "sr-mesh provisioner exited non-zero (run1=$status1 run2=$status2)"
        echo "$out1" | tail -20 | sed 's/^/  run1: /'
        echo "$out2" | tail -20 | sed 's/^/  run2: /'
    elif [ "$count_before" != "$count_after" ]; then
        fail "policy count changed between two consecutive mesh runs ($count_before -> $count_after) - not idempotent"
    else
        pass "sr-mesh provisioner is idempotent (policy count stayed at $count_after across two runs)"
    fi
fi

# ---------------------------------------------------------------------------
# Summary + cleanup
# ---------------------------------------------------------------------------
wait_for_synced_session 30 || true
cleanup_all

section "Summary"
echo "  Passed: $PASS_COUNT"
echo "  Warned: $WARN_COUNT"
echo "  Failed: $FAIL_COUNT"
if [ "$FAIL_COUNT" -eq 0 ]; then
    color_green "ALL TESTS PASSED"
    exit 0
else
    color_red "SOME TESTS FAILED"
    exit 1
fi
