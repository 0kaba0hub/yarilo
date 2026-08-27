#!/bin/sh
# Snapshot both backends the moment imaptest reports a stalled command.
#
# Why by event and not on a timer: the stall we are after lasted minutes but
# appeared twice in a day, and three scheduled captures all landed on a healthy
# server. A dump taken after the run says nothing -- the parked goroutine is
# gone by then.
#
#   hack/imaptest/stall-watch.sh <job-name> <out-dir> [probe-pod]
#
# Runs until the job ends. Every trigger writes goroutine dumps (debug=2, which
# names the parked stack), cgroup cpu.stat (throttling is the other suspect),
# and one 30s CPU profile. Captures are rate-limited to one per COOLDOWN so a
# run of stalls does not bury the first one.
set -u
JOB="${1:?job name}"
OUT="${2:-/tmp/stall-watch}"
PROBE="${3:-dig1}"
NS="${NS:-yarilo-sb}"
COOLDOWN="${COOLDOWN:-60}"

BACKENDS=$(kubectl -n "$NS" get pods -l app.kubernetes.io/component=backend \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.podIP} {end}' 2>/dev/null)
[ -z "$BACKENDS" ] && BACKENDS=$(kubectl -n "$NS" get pods --no-headers \
  | awk '/^yarilo-backend-[0-9]/ {print $1}' \
  | while read -r p; do printf '%s=%s ' "$p" "$(kubectl -n "$NS" get pod "$p" -o jsonpath='{.status.podIP}')"; done)

mkdir -p "$OUT"
echo "watch: job=$JOB backends=$BACKENDS" | tee "$OUT/watch.log"
kubectl -n "$NS" exec "$PROBE" -- mkdir -p /caps 2>/dev/null

last=0
capture() {
  tag="$1"
  for b in $BACKENDS; do
    name=${b%%=*}; ip=${b#*=}
    kubectl -n "$NS" exec "$PROBE" -- sh -c \
      "wget -qO /caps/$tag-$name-goroutine.txt 'http://$ip:8080/debug/pprof/goroutine?debug=2'" 2>/dev/null
    kubectl -n "$NS" exec "$name" -c yarilo-imap -- \
      cat /sys/fs/cgroup/cpu.stat > "$OUT/$tag-$name-cpu.stat" 2>/dev/null
  done
  first=${BACKENDS%%=*}; firstip=$(echo "$BACKENDS" | awk '{print $1}'); firstip=${firstip#*=}
  kubectl -n "$NS" exec "$PROBE" -- sh -c \
    "wget -qO /caps/$tag-cpu.pb.gz 'http://$firstip:8080/debug/pprof/profile?seconds=30'" 2>/dev/null &
  echo "$(date +%H:%M:%S) captured $tag" | tee -a "$OUT/watch.log"
}

kubectl -n "$NS" logs -f "job/$JOB" 2>/dev/null | while IFS= read -r line; do
  case "$line" in
    *"stalled >3s"*|*"stalled for"*)
      now=$(date +%s)
      [ $((now - last)) -lt "$COOLDOWN" ] && continue
      last=$now
      echo "$(date +%H:%M:%S) EVENT: $line" | tee -a "$OUT/watch.log"
      capture "s$now"
      ;;
  esac
done
echo "watch: the job ended" | tee -a "$OUT/watch.log"
