package monitor

import "time"

// Config holds yarilo-monitor settings loaded from YAML.
//
// Example:
//
//	director_addr: "127.0.0.1:9102"
//	interval: 10
//	timeout: 3
//	retry_count: 3
//	rapid_rounds: 10
//	rapid_fails_needed: 7
//
//	poll_imap: true
//	imap_port: 993
//	poll_pop3: false
//	pop3_port: 110
//	poll_lmtp: false
//	lmtp_port: 24
//
//	# Per-tag monitoring credentials.
//	# Tag is the join key between backends, users, storage, and monitor accounts.
//	tags:
//	  "":          # default / untagged backends
//	    user: monitor@example.com
//	    password: secret
//	  ssd:
//	    user: monitor-ssd@example.com
//	    password: ssd-secret
type Config struct {
	DirectorAddr     string `koanf:"director_addr"`
	Interval         int    `koanf:"interval"`           // seconds between polls; default 10
	Timeout          int    `koanf:"timeout"`            // seconds per probe attempt; default 3
	RetryCount       int    `koanf:"retry_count"`        // consecutive failures before rapid poll; default 3
	RapidRounds      int    `koanf:"rapid_rounds"`       // rapid poll iterations; default 10
	RapidFailsNeeded int    `koanf:"rapid_fails_needed"` // rapid poll failures to declare down; default 7

	// Protocol probes — applied to every backend in the ring.
	PollIMAP bool `koanf:"poll_imap"`
	IMAPPort int  `koanf:"imap_port"` // default 993
	PollPOP3 bool `koanf:"poll_pop3"`
	POP3Port int  `koanf:"pop3_port"` // default 110
	PollLMTP bool `koanf:"poll_lmtp"`
	LMTPPort int  `koanf:"lmtp_port"` // default 24

	// Tags maps tag name → monitoring credentials.
	// The tag is the same label used on backends, users, and storage.
	// The empty string key "" covers untagged backends.
	Tags map[string]TagConfig `koanf:"tags"`
}

// TagConfig holds the monitoring account for one tag.
type TagConfig struct {
	User     string `koanf:"user"`
	Password string `koanf:"password"`
}

func (c *Config) interval() time.Duration {
	if c.Interval <= 0 {
		return 10 * time.Second
	}
	return time.Duration(c.Interval) * time.Second
}

func (c *Config) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 3 * time.Second
	}
	return time.Duration(c.Timeout) * time.Second
}

func (c *Config) retryCount() int {
	if c.RetryCount <= 0 {
		return 3
	}
	return c.RetryCount
}

func (c *Config) rapidRounds() int {
	if c.RapidRounds <= 0 {
		return 10
	}
	return c.RapidRounds
}

func (c *Config) rapidFailsNeeded() int {
	if c.RapidFailsNeeded <= 0 {
		return 7
	}
	return c.RapidFailsNeeded
}

func (c *Config) imapPort() int {
	if c.IMAPPort <= 0 {
		return 993
	}
	return c.IMAPPort
}

func (c *Config) pop3Port() int {
	if c.POP3Port <= 0 {
		return 110
	}
	return c.POP3Port
}

func (c *Config) lmtpPort() int {
	if c.LMTPPort <= 0 {
		return 24
	}
	return c.LMTPPort
}

// credentials returns the user/password for the given tag.
// Falls back to the "" (default) tag entry if the specific tag has no entry.
// Returns empty strings if no credentials are configured for the tag.
func (c *Config) credentials(tag string) (user, pass string) {
	if tc, ok := c.Tags[tag]; ok {
		return tc.User, tc.Password
	}
	if tc, ok := c.Tags[""]; ok {
		return tc.User, tc.Password
	}
	return "", ""
}
