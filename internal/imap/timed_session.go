package imap

import (
	"errors"
	"log/slog"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// timedSession is the one seam every command passes through on its way back to
// the client, and it does two things there: times the command, and classifies
// the error.
//
// Classifying here rather than at each call site is what makes it complete. Per
// site, the answer is always one site behind -- STORE was classified and the
// failure arrived from a different call in the same handler, so a lock-service
// restart still reached clients as SERVERBUG (#1339). Here every command is
// covered, and a command added later cannot miss it: the interface assertion
// below stops the build until it is forwarded.
//
// It does not embed the session on purpose. Embedding would inherit any method
// this file forgets, and the miss would be invisible: the command would work,
// never be measured, and never be classified. Written out, a forgotten method
// fails the assertion and the build stops.
type timedSession struct{ s *session }

var _ imapserver.SessionIMAP4rev2 = (*timedSession)(nil)

// observe records one command. The driver is a label because the question this
// exists to answer is comparative -- the same command costs different amounts
// on different storage, and that difference is the thing being hunted.
func (t *timedSession) observe(command string, start time.Time) {
	driver := "unknown"
	if t.s.userInfo != nil && t.s.userInfo.Driver != "" {
		driver = t.s.userInfo.Driver
	}
	metricCommandSeconds.WithLabelValues(command, driver).Observe(time.Since(start).Seconds())
}

// classify turns a store failure into the answer a client can act on, and says
// in the log what the client cannot be told.
//
// The rule the log line follows, for whoever adds the next soft degradation:
// the diagnosis is needed MOST where the service degrades gently. A failure
// that takes everything down is found in seconds; one folder refusing inside a
// working account is found only if the server names it. That is why this line
// carries the account, the folder and the session, and why the JMAP side --
// which fails the whole account at once -- had them first and this side did not
// (#1344).
func (t *timedSession) classify(err error) error {
	out := dependencyError(err)
	var corrupt *mailbox.CorruptIndexError
	if errors.As(err, &corrupt) {
		user := ""
		if t.s.userInfo != nil {
			user = t.s.userInfo.Username
		}
		slog.Error("imap: folder index is unreadable, refusing this folder only",
			"user", user, "sid", t.s.sid, "folder", corrupt.Folder, "err", corrupt.Err)
	}
	return out
}

// SessionID, Close and AuthenticateMechanisms are plumbing rather than
// commands: they answer no client request of their own.
func (t *timedSession) SessionID() string                { return t.s.SessionID() }
func (t *timedSession) Close() error                     { return t.s.Close() }
func (t *timedSession) AuthenticateMechanisms() []string { return t.s.AuthenticateMechanisms() }

func (t *timedSession) GetACL(folder string) (*imaplib.GetACLData, error) {
	defer t.observe("GetACL", time.Now())
	v, err := t.s.GetACL(folder)
	return v, t.classify(err)
}

func (t *timedSession) MyRights(folder string) (*imaplib.MyRightsData, error) {
	defer t.observe("MyRights", time.Now())
	v, err := t.s.MyRights(folder)
	return v, t.classify(err)
}

func (t *timedSession) ListRights(folder string, identifier imaplib.RightsIdentifier) (*imaplib.ListRightsData, error) {
	defer t.observe("ListRights", time.Now())
	v, err := t.s.ListRights(folder, identifier)
	return v, t.classify(err)
}

func (t *timedSession) SetACL(folder string, identifier imaplib.RightsIdentifier, modification imaplib.RightModification, rights imaplib.RightSet) error {
	defer t.observe("SetACL", time.Now())
	return dependencyError(t.s.SetACL(folder, identifier, modification, rights))
}

func (t *timedSession) DeleteACL(folder string, identifier imaplib.RightsIdentifier) error {
	defer t.observe("DeleteACL", time.Now())
	return dependencyError(t.s.DeleteACL(folder, identifier))
}

func (t *timedSession) GetQuotaRoot(mailbox string) (*imaplib.QuotaRootData, error) {
	defer t.observe("GetQuotaRoot", time.Now())
	v, err := t.s.GetQuotaRoot(mailbox)
	return v, t.classify(err)
}

func (t *timedSession) GetQuota(root string) (*imaplib.QuotaData, error) {
	defer t.observe("GetQuota", time.Now())
	v, err := t.s.GetQuota(root)
	return v, t.classify(err)
}

func (t *timedSession) Login(username, password string) error {
	defer t.observe("Login", time.Now())
	return dependencyError(t.s.Login(username, password))
}

func (t *timedSession) Authenticate(mech string) (sasl.Server, error) {
	defer t.observe("Authenticate", time.Now())
	v, err := t.s.Authenticate(mech)
	return v, t.classify(err)
}

func (t *timedSession) Select(name string, opts *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	defer t.observe("Select", time.Now())
	v, err := t.s.Select(name, opts)
	return v, t.classify(err)
}

func (t *timedSession) Unselect() error {
	defer t.observe("Unselect", time.Now())
	return dependencyError(t.s.Unselect())
}

func (t *timedSession) Create(name string, opts *imaplib.CreateOptions) error {
	defer t.observe("Create", time.Now())
	return dependencyError(t.s.Create(name, opts))
}

func (t *timedSession) Delete(name string) error {
	defer t.observe("Delete", time.Now())
	return dependencyError(t.s.Delete(name))
}

func (t *timedSession) Rename(oldName, newName string, opts *imaplib.RenameOptions) error {
	defer t.observe("Rename", time.Now())
	return dependencyError(t.s.Rename(oldName, newName, opts))
}

func (t *timedSession) Subscribe(name string) error {
	defer t.observe("Subscribe", time.Now())
	return dependencyError(t.s.Subscribe(name))
}

func (t *timedSession) Unsubscribe(name string) error {
	defer t.observe("Unsubscribe", time.Now())
	return dependencyError(t.s.Unsubscribe(name))
}

func (t *timedSession) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) error {
	defer t.observe("List", time.Now())
	return dependencyError(t.s.List(w, ref, patterns, opts))
}

func (t *timedSession) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	defer t.observe("Status", time.Now())
	v, err := t.s.Status(name, opts)
	return v, t.classify(err)
}

func (t *timedSession) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
	defer t.observe("Append", time.Now())
	v, err := t.s.Append(name, r, opts)
	return v, t.classify(err)
}

func (t *timedSession) Notify(w *imapserver.UpdateWriter, options *imaplib.NotifyOptions) error {
	defer t.observe("Notify", time.Now())
	return dependencyError(t.s.Notify(w, options))
}

func (t *timedSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	defer t.observe("Poll", time.Now())
	return dependencyError(t.s.Poll(w, allowExpunge))
}

func (t *timedSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	defer t.observe("Idle", time.Now())
	return dependencyError(t.s.Idle(w, stop))
}

func (t *timedSession) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	defer t.observe("Expunge", time.Now())
	return dependencyError(t.s.Expunge(w, uids))
}

func (t *timedSession) Search(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, opts *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	defer t.observe("Search", time.Now())
	v, err := t.s.Search(kind, criteria, opts)
	return v, t.classify(err)
}

func (t *timedSession) Fetch(w *imapserver.FetchWriter, numSet imaplib.NumSet, opts *imaplib.FetchOptions) error {
	defer t.observe("Fetch", time.Now())
	return dependencyError(t.s.Fetch(w, numSet, opts))
}

func (t *timedSession) Store(w *imapserver.FetchWriter, numSet imaplib.NumSet, storeFlags *imaplib.StoreFlags, opts *imaplib.StoreOptions) error {
	defer t.observe("Store", time.Now())
	return dependencyError(t.s.Store(w, numSet, storeFlags, opts))
}

func (t *timedSession) Copy(numSet imaplib.NumSet, dest string) (*imaplib.CopyData, error) {
	defer t.observe("Copy", time.Now())
	v, err := t.s.Copy(numSet, dest)
	return v, t.classify(err)
}

// ID answers RFC 2971. It takes no error path -- the answer is configuration,
// not a lookup -- but it goes through the same seam as every other command so
// the structural guard stays true and the timing has no hole.
func (t *timedSession) ID(clientID *imaplib.IDData) *imaplib.IDData {
	defer t.observe("ID", time.Now())
	return t.s.ID(clientID)
}

func (t *timedSession) Namespace() (*imaplib.NamespaceData, error) {
	defer t.observe("Namespace", time.Now())
	v, err := t.s.Namespace()
	return v, t.classify(err)
}

func (t *timedSession) GetMetadata(folder string, entries []string, opts *imaplib.GetMetadataOptions) (*imaplib.GetMetadataData, error) {
	defer t.observe("GetMetadata", time.Now())
	v, err := t.s.GetMetadata(folder, entries, opts)
	return v, t.classify(err)
}

func (t *timedSession) SetMetadata(folder string, entries map[string]*[]byte) error {
	defer t.observe("SetMetadata", time.Now())
	return dependencyError(t.s.SetMetadata(folder, entries))
}

func (t *timedSession) Move(w *imapserver.MoveWriter, numSet imaplib.NumSet, dest string) error {
	defer t.observe("Move", time.Now())
	return dependencyError(t.s.Move(w, numSet, dest))
}
