package jmap

import (
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/jmapcore"
)

// LimitsFrom renders the advertised bounds from configuration. The mapping
// lives here rather than in pkg/config so the config package stays free of the
// protocol layer, and jmapcore free of yarilo.
func LimitsFrom(c config.JMAPProtocolConfig) jmapcore.Limits {
	return jmapcore.Limits{
		BaseURL:               c.BaseURL,
		MaxSizeUpload:         c.MaxSizeUpload,
		MaxSizeRequest:        c.MaxSizeRequest,
		MaxConcurrentRequests: c.MaxConcurrentRequests,
		MaxCallsInRequest:     c.MaxCallsInRequest,
		MaxObjectsInGet:       c.MaxObjectsInGet,
		MaxObjectsInSet:       c.MaxObjectsInSet,
	}
}
