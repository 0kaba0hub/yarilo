package imap

// SetTestSessionID stands in for the login proxy's preamble, so a test
// connection can carry a session id and the two owner spellings stay
// distinguishable (#1652).
func SetTestSessionID(id string) { testSessionID = id }
