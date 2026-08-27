"""Snapshot the backends the moment imaptest reports a stalled command.

Runs inside the cluster so that a broken link to the operator's machine cannot
end the watch: it reads the imaptest log through the API server with its own
service account, and writes every capture to a volume that outlives the pod.

Why by event: the stall it hunts lasted minutes but appeared twice in a day,
and three captures taken on a timer all landed on a healthy server. A dump
taken after the run says nothing -- the parked goroutine is gone by then.

Per trigger it writes, for each backend, the full goroutine dump (debug=2,
which names the parked stack), a metrics snapshot, the container's cgroup
cpu.stat when the node's cgroupfs is mounted, and one CPU profile. Captures
are rate-limited so a burst of stalls cannot bury the first one.
"""
import json, os, re, ssl, sys, time, urllib.request, glob

NS = os.environ.get("NS", "yarilo-sb")
TARGET = os.environ["TARGET"]            # job-name label of the run to watch
OUT = os.environ.get("OUT", "/caps")
COOLDOWN = int(os.environ.get("COOLDOWN", "60"))
PROFILE_SECS = int(os.environ.get("PROFILE_SECS", "30"))
TELEMETRY_PORT = os.environ.get("TELEMETRY_PORT", "8080")
PATTERN = re.compile(os.environ.get("PATTERN", r"stalled (>3s|for \d+ secs)"))

API = "https://kubernetes.default.svc"
SA = "/var/run/secrets/kubernetes.io/serviceaccount"
TOKEN = open(SA + "/token").read().strip()
CTX = ssl.create_default_context(cafile=SA + "/ca.crt")


def api(path, stream=False):
    req = urllib.request.Request(API + path, headers={"Authorization": "Bearer " + TOKEN})
    r = urllib.request.urlopen(req, context=CTX, timeout=None if stream else 30)
    return r if stream else json.load(r)


def log(msg):
    line = "%s %s" % (time.strftime("%H:%M:%S"), msg)
    print(line, flush=True)
    with open(OUT + "/watch.log", "a") as f:
        f.write(line + "\n")


def backends():
    pods = api("/api/v1/namespaces/%s/pods" % NS)["items"]
    out = []
    for p in pods:
        name = p["metadata"]["name"]
        if name.startswith("yarilo-backend-") and p["status"].get("podIP"):
            out.append((name, p["status"]["podIP"], p["metadata"]["uid"]))
    return sorted(out)


def target_pod():
    # The run may not have been scheduled yet when the watch starts.
    for _ in range(120):
        q = "/api/v1/namespaces/%s/pods?labelSelector=job-name%%3D%s" % (NS, TARGET)
        for p in api(q)["items"]:
            if p["status"]["phase"] in ("Running", "Succeeded"):
                return p["metadata"]["name"]
        time.sleep(5)
    raise SystemExit("the run %s never started" % TARGET)


def fetch(url, path, timeout=60):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r, open(path, "wb") as f:
            f.write(r.read())
        return True
    except Exception as e:                      # a capture must not end the watch
        log("  %s: %s" % (url, e))
        return False


def cpu_stat(uid, path):
    # Only when the node's cgroupfs is mounted read-only into this pod; the
    # slice directory carries the pod UID with dashes turned into underscores.
    for pat in ("/host/sys/fs/cgroup/kubepods.slice/*/*pod%s.slice/*/cpu.stat",
                "/host/sys/fs/cgroup/kubepods/*/pod%s/*/cpu.stat"):
        for hit in glob.glob(pat % uid.replace("-", "_")) + glob.glob(pat % uid):
            open(path, "w").write(open(hit).read())
            return True
    return False


def capture(tag, bes):
    for name, ip, uid in bes:
        base = "%s/%s-%s" % (OUT, tag, name)
        fetch("http://%s:%s/debug/pprof/goroutine?debug=2" % (ip, TELEMETRY_PORT), base + "-goroutine.txt")
        fetch("http://%s:%s/metrics" % (ip, TELEMETRY_PORT), base + "-metrics.txt")
        cpu_stat(uid, base + "-cpu.stat")
    name, ip, _ = bes[0]
    fetch("http://%s:%s/debug/pprof/profile?seconds=%d" % (ip, TELEMETRY_PORT, PROFILE_SECS),
          "%s/%s-%s-cpu.pb.gz" % (OUT, tag, name), timeout=PROFILE_SECS + 30)
    log("captured %s" % tag)


def main():
    os.makedirs(OUT, exist_ok=True)
    bes = backends()
    pod = target_pod()
    log("watching %s (pod %s), backends: %s" % (TARGET, pod, ", ".join(n for n, _, _ in bes)))
    stream = api("/api/v1/namespaces/%s/pods/%s/log?follow=true&sinceSeconds=3600" % (NS, pod), stream=True)
    last, hits = 0.0, 0
    for raw in stream:
        line = raw.decode("utf-8", "replace").rstrip()
        if not PATTERN.search(line):
            continue
        hits += 1
        now = time.time()
        if now - last < COOLDOWN:
            continue
        last = now
        log("EVENT: %s" % line.strip()[:200])
        capture("s%d" % int(now), bes)
    log("run ended; %d stall lines seen" % hits)


main()
