package imap

import (
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-sasl"
)

// timedSession times every command a client issues, at the one seam every
// command passes through.
//
// It does not embed the session on purpose. Embedding would inherit any method
// this file forgets, and the miss would be invisible: the command would work
// and simply never be measured. Written out, a forgotten method fails the
// interface assertion below and the build stops.
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

// SessionID, Close and AuthenticateMechanisms are plumbing rather than
// commands: they answer no client request of their own.
func (t *timedSession) SessionID() string                { return t.s.SessionID() }
func (t *timedSession) Close() error                     { return t.s.Close() }
func (t *timedSession) AuthenticateMechanisms() []string { return t.s.AuthenticateMechanisms() }

func (t *timedSession) GetACL(folder string) (*imaplib.GetACLData, error) {
	defer t.observe("GetACL", time.Now())
	return t.s.GetACL(folder)
}

func (t *timedSession) MyRights(folder string) (*imaplib.MyRightsData, error) {
	defer t.observe("MyRights", time.Now())
	return t.s.MyRights(folder)
}

func (t *timedSession) ListRights(folder string, identifier imaplib.RightsIdentifier) (*imaplib.ListRightsData, error) {
	defer t.observe("ListRights", time.Now())
	return t.s.ListRights(folder, identifier)
}

func (t *timedSession) SetACL(folder string, identifier imaplib.RightsIdentifier, modification imaplib.RightModification, rights imaplib.RightSet) error {
	defer t.observe("SetACL", time.Now())
	return t.s.SetACL(folder, identifier, modification, rights)
}

func (t *timedSession) DeleteACL(folder string, identifier imaplib.RightsIdentifier) error {
	defer t.observe("DeleteACL", time.Now())
	return t.s.DeleteACL(folder, identifier)
}

func (t *timedSession) GetQuotaRoot(mailbox string) (*imaplib.QuotaRootData, error) {
	defer t.observe("GetQuotaRoot", time.Now())
	return t.s.GetQuotaRoot(mailbox)
}

func (t *timedSession) GetQuota(root string) (*imaplib.QuotaData, error) {
	defer t.observe("GetQuota", time.Now())
	return t.s.GetQuota(root)
}

func (t *timedSession) Login(username, password string) error {
	defer t.observe("Login", time.Now())
	return t.s.Login(username, password)
}

func (t *timedSession) Authenticate(mech string) (sasl.Server, error) {
	defer t.observe("Authenticate", time.Now())
	return t.s.Authenticate(mech)
}

func (t *timedSession) Select(name string, opts *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	defer t.observe("Select", time.Now())
	return t.s.Select(name, opts)
}

func (t *timedSession) Unselect() error {
	defer t.observe("Unselect", time.Now())
	return t.s.Unselect()
}

func (t *timedSession) Create(name string, opts *imaplib.CreateOptions) error {
	defer t.observe("Create", time.Now())
	return t.s.Create(name, opts)
}

func (t *timedSession) Delete(name string) error {
	defer t.observe("Delete", time.Now())
	return t.s.Delete(name)
}

func (t *timedSession) Rename(oldName, newName string, opts *imaplib.RenameOptions) error {
	defer t.observe("Rename", time.Now())
	return t.s.Rename(oldName, newName, opts)
}

func (t *timedSession) Subscribe(name string) error {
	defer t.observe("Subscribe", time.Now())
	return t.s.Subscribe(name)
}

func (t *timedSession) Unsubscribe(name string) error {
	defer t.observe("Unsubscribe", time.Now())
	return t.s.Unsubscribe(name)
}

func (t *timedSession) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) error {
	defer t.observe("List", time.Now())
	return t.s.List(w, ref, patterns, opts)
}

func (t *timedSession) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	defer t.observe("Status", time.Now())
	return t.s.Status(name, opts)
}

func (t *timedSession) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
	defer t.observe("Append", time.Now())
	return t.s.Append(name, r, opts)
}

func (t *timedSession) Notify(w *imapserver.UpdateWriter, options *imaplib.NotifyOptions) error {
	defer t.observe("Notify", time.Now())
	return t.s.Notify(w, options)
}

func (t *timedSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	defer t.observe("Poll", time.Now())
	return t.s.Poll(w, allowExpunge)
}

func (t *timedSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	defer t.observe("Idle", time.Now())
	return t.s.Idle(w, stop)
}

func (t *timedSession) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	defer t.observe("Expunge", time.Now())
	return t.s.Expunge(w, uids)
}

func (t *timedSession) Search(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, opts *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	defer t.observe("Search", time.Now())
	return t.s.Search(kind, criteria, opts)
}

func (t *timedSession) Fetch(w *imapserver.FetchWriter, numSet imaplib.NumSet, opts *imaplib.FetchOptions) error {
	defer t.observe("Fetch", time.Now())
	return t.s.Fetch(w, numSet, opts)
}

func (t *timedSession) Store(w *imapserver.FetchWriter, numSet imaplib.NumSet, storeFlags *imaplib.StoreFlags, opts *imaplib.StoreOptions) error {
	defer t.observe("Store", time.Now())
	return t.s.Store(w, numSet, storeFlags, opts)
}

func (t *timedSession) Copy(numSet imaplib.NumSet, dest string) (*imaplib.CopyData, error) {
	defer t.observe("Copy", time.Now())
	return t.s.Copy(numSet, dest)
}

func (t *timedSession) Namespace() (*imaplib.NamespaceData, error) {
	defer t.observe("Namespace", time.Now())
	return t.s.Namespace()
}

func (t *timedSession) GetMetadata(folder string, entries []string, opts *imaplib.GetMetadataOptions) (*imaplib.GetMetadataData, error) {
	defer t.observe("GetMetadata", time.Now())
	return t.s.GetMetadata(folder, entries, opts)
}

func (t *timedSession) SetMetadata(folder string, entries map[string]*[]byte) error {
	defer t.observe("SetMetadata", time.Now())
	return t.s.SetMetadata(folder, entries)
}

func (t *timedSession) Move(w *imapserver.MoveWriter, numSet imaplib.NumSet, dest string) error {
	defer t.observe("Move", time.Now())
	return t.s.Move(w, numSet, dest)
}
