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
	imapUser := *flagPasswdFileUser
	imapPass := *flagPasswdFilePass
	imap := surfaceState{surface: surfIMAP, present: imapUser != "", needs: "-passwd-file-user"}
	jmap := surfaceState{surface: surfJMAP, present: *flagJMAP && *flagJMAPUser != "", needs: "-jmap and -jmap-user"}
	delivery := surfaceState{surface: surfLMTP, present: *flagDeliveryPort != "", needs: "-delivery-port"}

	row := func(name string, fn func() error, sides ...surfaceState) {
		if enabled, skip := rowGate(sides...); enabled {
			*checks = append(*checks, check{area: consistencyArea, name: name, fn: fn})
		} else {
			*checks = append(*checks, check{area: consistencyArea, name: name, skip: skip})
		}
	}

	row("consistency imap<->jmap identity (id, subject, size, internal date)",
		func() error { return checkConsistencyIdentity(imapUser, imapPass) },
		imap, jmap, delivery)

	row("consistency imap<->jmap flag and keyword visibility (both directions)",
		func() error { return checkConsistencyFlags(imapUser, imapPass) },
		imap, jmap, delivery)
}
