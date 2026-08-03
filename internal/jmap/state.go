package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/yarilomail/yarilo/pkg/config"
)

// sessionState is the session resource's "state" string (RFC 8620 §2). A client
// caches the session until it changes, so it must move with the advertised
// values and stay put otherwise.
func sessionState(cfg config.JMAPProtocolConfig) string {
	var b strings.Builder
	for _, v := range []int64{
		cfg.MaxSizeUpload,
		cfg.MaxSizeRequest,
		int64(cfg.MaxConcurrentRequests),
		int64(cfg.MaxCallsInRequest),
		int64(cfg.MaxObjectsInGet),
		int64(cfg.MaxObjectsInSet),
	} {
		b.WriteString(strconv.FormatInt(v, 10))
		b.WriteByte(';')
	}
	b.WriteString(cfg.BaseURL)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
