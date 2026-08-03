package jmapcore

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ProblemBlank is the type of a request-level error with no JMAP-specific
// meaning beyond its status code.
const ProblemBlank = "about:blank"

// JMAP names its own problem types under this prefix (RFC 8620 §3.6.1).
const (
	ProblemUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	ProblemNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	ProblemNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	ProblemLimit             = "urn:ietf:params:jmap:error:limit"
)

// WriteJSON emits a response body. A failure mid-write cannot be signalled to
// the client, the status line is already gone, so it is only logged.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("jmapcore: response write failed", "err", err)
	}
}

// WriteProblem emits the RFC 7807 body JMAP uses for request-level errors
// (RFC 8620 §3.6.1), with no JMAP-specific type.
func WriteProblem(w http.ResponseWriter, status int, detail string) {
	WriteProblemType(w, status, ProblemBlank, detail)
}

// WriteProblemType emits an RFC 7807 body naming a specific problem type.
func WriteProblemType(w http.ResponseWriter, status int, problemType, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	body := map[string]any{"type": problemType, "status": status, "detail": detail}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Debug("jmapcore: problem write failed", "err", err)
	}
}
