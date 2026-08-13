package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Row: quota numbers agree between IMAP and the admin API. Both report the same
// account's usage, computed by the same index, and a divergence means one of
// them is reading a stale or differently-scoped total — which a per-surface
// check cannot see, because each is self-consistent.
//
// The units differ by protocol and are converted at read time, deliberately and
// visibly: IMAP QUOTA counts kibibytes (RFC 9208), and the admin API reports
// both bytes and KiB. Converting is not normalising away a difference — the
// number of kibibytes is the same fact in both, and only its rendering differs.
//
// JMAP has no side here: the quota extension (urn:ietf:params:jmap:quota) is
// not implemented, so the pair registers as a skip naming it rather than
// quietly not existing. When it lands, the skip becomes a check.
func checkConsistencyQuota(user string) error {
	left, err := imapReadQuota(user)
	if err != nil {
		return fmt.Errorf("read quota over imap: %w", err)
	}
	right, err := adminReadQuota(user)
	if err != nil {
		return fmt.Errorf("read quota over the admin API: %w", err)
	}
	return judgeRow("imap<->admin API quota", left, right, defaultAllowances())
}

func imapReadQuota(user string) (*reading, error) {
	_, pass := consistencyAccount()
	c, err := imapDial()
	if err != nil {
		return nil, err
	}
	defer c.close()
	if err := c.login(user, pass); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	lines, err := c.cmd("GETQUOTAROOT INBOX")
	if err != nil {
		return nil, fmt.Errorf("getquotaroot: %w", err)
	}
	usedKiB, limitKiB, ok := parseQuotaStorage(lines)
	if !ok {
		return nil, fmt.Errorf("no STORAGE quota in %q", strings.Join(lines, " | "))
	}
	return newReading(surfIMAP).
		field("storageUsedKiB", strconv.FormatInt(usedKiB, 10)).
		field("storageLimitKiB", strconv.FormatInt(limitKiB, 10)), nil
}

// parseQuotaStorage reads the STORAGE resource out of a QUOTA response:
// * QUOTA "<root>" (STORAGE <used> <limit> ...)
func parseQuotaStorage(lines []string) (used, limit int64, ok bool) {
	for _, l := range lines {
		i := strings.Index(l, "STORAGE ")
		if !strings.HasPrefix(l, "* QUOTA ") || i < 0 {
			continue
		}
		fields := strings.Fields(strings.TrimRight(l[i+len("STORAGE "):], ")"))
		if len(fields) < 2 {
			continue
		}
		u, err1 := strconv.ParseInt(fields[0], 10, 64)
		lim, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return u, lim, true
	}
	return 0, 0, false
}

func adminReadQuota(user string) (*reading, error) {
	url := strings.TrimRight(*flagBackendAPI, "/") + "/api/backend/quota/show?user=" + user
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := backendAPIToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := jmapClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota/show: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		StorageValue int64 `json:"storage_value"` // used storage in KiB, the same unit IMAP reports
		StorageLimit int64 `json:"storage_limit"` // KiB; -1 = unlimited
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode quota/show: %w (%s)", err, strings.TrimSpace(string(body)))
	}
	// An unlimited quota is -1 over the admin API and 0 over IMAP: two
	// spellings of "no limit", converted here rather than tolerated by the
	// judge, because the conversion is exact and only one of them is a number.
	limit := out.StorageLimit
	if limit < 0 {
		limit = 0
	}
	return newReading(surfAdminAPI).
		field("storageUsedKiB", strconv.FormatInt(out.StorageValue, 10)).
		field("storageLimitKiB", strconv.FormatInt(limit, 10)), nil
}

// backendAPIToken reads the bearer token the same way the director check reads
// its own: flag first, then the service-specific env var, then the shared admin
// one. A deployment that sets only YARILO_ADMIN_TOKEN works without a flag.
func backendAPIToken() string {
	if *flagBackendAPIToken != "" {
		return *flagBackendAPIToken
	}
	if t := os.Getenv("BACKEND_API_TOKEN"); t != "" {
		return t
	}
	return os.Getenv("YARILO_ADMIN_TOKEN")
}
