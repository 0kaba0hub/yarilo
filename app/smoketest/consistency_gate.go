package main

import (
	"fmt"
	"sort"
	"strings"
)

// A consistency row enables itself when the deployment configured BOTH surfaces
// it compares. One side present and the other absent is a visible skip naming
// what is missing, never a silent omission — the property #1197 established for
// the gate as a whole, applied per row. -require-all and -require-all-except
// carry the operator's choice about it unchanged, because a skipped row is an
// ordinary skipped check in the ordinary area machinery.

// surfaceState is one side of a row and whether the deployment configured it.
type surfaceState struct {
	surface surface
	present bool
	// needs names the flag that would turn it on, quoted into the skip so the
	// operator reads what to pass rather than what is missing in the abstract.
	needs string
}

// rowGate reports whether every side of a row is configured, and the skip text
// when it is not. The text names the missing surfaces and the flags that would
// supply them; a row missing two sides says both, because fixing one and
// re-running to discover the other is a worse gate.
func rowGate(sides ...surfaceState) (enabled bool, skip string) {
	var missing []string
	for _, s := range sides {
		if !s.present {
			missing = append(missing, fmt.Sprintf("%s (%s)", s.surface, s.needs))
		}
	}
	if len(missing) == 0 {
		return true, ""
	}
	sort.Strings(missing)
	return false, "needs " + strings.Join(missing, " and ")
}

// consistencyArea is the area name the rows register under, so the summary
// reports them beside every other area and -require-all-except can name them.
const consistencyArea = "consistency"

// registerConsistency adds the pair matrix. Every row states its sides; a row
// whose sides are not all configured registers as a skip naming them, which is
// what makes forgetting a protocol impossible: adding one adds a row that is
// either checked or visibly skipped.
func registerConsistency(checks *[]check) {
	// One account read through both surfaces. Using the IMAP passdb user on
	// one side and the JMAP user on the other would compare two mailboxes and
	// call their difference a disagreement, so the rows log into IMAP as the
	// JMAP account and -jmap-user is what enables both sides.
	imapUser, imapPass := consistencyAccount()
	imap := surfaceState{surface: surfIMAP, present: imapUser != "",
		needs: "-jmap-user (the rows read one account through both surfaces)"}
	jmap := surfaceState{surface: surfJMAP, present: *flagJMAP && *flagJMAPUser != "", needs: "-jmap and -jmap-user"}
	delivery := surfaceState{surface: surfLMTP, present: *flagDeliveryPort != "", needs: "-delivery-port"}

	row := func(name string, fn func() error, sides ...surfaceState) {
		if enabled, skip := rowGate(sides...); enabled {
			*checks = append(*checks, check{area: consistencyArea, name: name, fn: fn})
		} else {
			*checks = append(*checks, check{area: consistencyArea, name: name, skip: skip})
		}
	}

	fts := surfaceState{surface: "fts", present: *flagFTSUser != "", needs: "-fts-user"}

	row("consistency imap<->jmap identity (id, subject, size, internal date)",
		func() error { return checkConsistencyIdentity(imapUser, imapPass) },
		imap, jmap, delivery)

	// Delivery writes the identity, and nothing can add it afterwards: the
	// header is part of the stored bytes. Only a live delivery can show it.
	row("consistency imap<->jmap message-id of a message delivered without one",
		func() error { return checkConsistencyMessageID(imapUser, imapPass) },
		imap, jmap, delivery)

	row("consistency imap->jmap flag becomes the keyword",
		func() error { return checkConsistencyFlagsIMAPToJMAP(imapUser, imapPass) },
		imap, jmap, delivery)

	// The write half of JMAP does not exist yet (#712). The row is registered
	// so the report keeps asking for it: a direction that quietly stops being
	// checked is the same hole this area was built to close.
	jmapWrite := surfaceState{surface: "jmap Email/set", present: false,
		needs: "Email/set, the JMAP write path — not implemented yet (#712)"}
	row("consistency jmap->imap keyword becomes the flag",
		func() error { return checkConsistencyFlagsJMAPToIMAP(imapUser, imapPass) },
		imap, jmapWrite, delivery)

	row("consistency imap SEARCH <-> jmap Email/query over one term",
		func() error { return checkConsistencySearch(imapUser, imapPass) },
		imap, jmap, delivery, fts)

	// The readers row needs the delivery and at least one reader beyond IMAP;
	// which readers are configured it discovers itself, and a reader that is
	// not deployed is simply not among them.
	row("consistency one lmtp delivery seen by every configured reader",
		func() error { return checkConsistencyReaders(imapUser, imapPass) },
		imap, delivery)

	// The reference deployment serves the admin API with mutual TLS, so the
	// skip names the certificate flags too: an operator who passes only
	// -backend-api against such an endpoint gets a handshake failure, and the
	// skip is the place that could have said so first (#1280).
	admin := surfaceState{surface: surfAdminAPI, present: *flagBackendAPI != "",
		needs: "-backend-api (plus -backend-api-cert/-backend-api-key when it is served over mTLS, as the reference deployment serves it)"}
	row("consistency imap<->admin API quota numbers",
		func() error { return checkConsistencyQuota(imapUser) },
		imap, admin)

	// JMAP has no quota surface yet (urn:ietf:params:jmap:quota is not
	// implemented). The row exists so that absence is reported every run
	// instead of being a pair nobody remembers is missing; when the extension
	// lands, this becomes a check.
	jmapQuota := surfaceState{surface: "jmap quota", present: false,
		needs: "urn:ietf:params:jmap:quota, not implemented yet"}
	row("consistency imap<->jmap quota numbers",
		func() error { return fmt.Errorf("unreachable: the jmap quota surface does not exist") },
		imap, jmapQuota)

	sieve := surfaceState{surface: surfManageSieve, present: *flagManageSieve && *flagManageSieveUser != "",
		needs: "-managesieve and -managesieve-user"}
	row("consistency managesieve script takes effect on the next delivery",
		func() error { return checkConsistencySieve(imapUser, imapPass) },
		sieve, imap, delivery)
}

// consistencyAccount is the account every row reads through both surfaces.
func consistencyAccount() (user, pass string) {
	return *flagJMAPUser, *flagJMAPPass
}
