package jmapcore

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// SessionState is the session resource's "state" string (RFC 8620 §2). A client
// caches the session until it changes, so it moves with the advertised values
// and stays put otherwise.
func SessionState(lim Limits) string {
	var b strings.Builder
	for _, v := range []int64{
		lim.MaxSizeUpload,
		lim.MaxSizeRequest,
		int64(lim.MaxConcurrentRequests),
		int64(lim.MaxCallsInRequest),
		int64(lim.MaxObjectsInGet),
		int64(lim.MaxObjectsInSet),
	} {
		b.WriteString(strconv.FormatInt(v, 10))
		b.WriteByte(';')
	}
	b.WriteString(lim.BaseURL)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
