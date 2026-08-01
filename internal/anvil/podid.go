package anvil

import "os"

// podID identifies this anvil replica in structured logs, so a cross-replica
// kick can be traced directly (SUBSCRIBE served by pod-A, EMIT arrived on pod-B,
// delivered by pod-A) instead of guessing ClusterIP routing. Resolved ONCE at
// package load from HOSTNAME (the pod name in k8s), never re-read per log line.
var podID = resolvePodID()

func resolvePodID() string {
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}
