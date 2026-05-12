// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the yarilo IMAP server.
type Server struct {
	srv  *imapserver.Server
	opts Options
}

// Options configures the IMAP server.
type Options struct {
	Addr      string // TLS address, e.g. ":993"
	AddrPlain string // STARTTLS address, e.g. ":143"
	TLSConfig *tls.Config
	Mailbox   mailbox.MailboxBackend
	Index     mailbox.IndexBackend
	Auth      protocol.Passdb
}

// New creates an IMAP server.
func New(opts Options) *Server {
	s := &Server{opts: opts}

	caps := imaplib.CapSet{
		imaplib.CapIMAP4rev1:   {},
		imaplib.CapIMAP4rev2:   {},
		imaplib.CapIdle:        {},
		imaplib.CapMove:        {},
		imaplib.CapCondStore:   {},
		imaplib.CapUIDPlus:     {},
		imaplib.CapNamespace:   {},
		imaplib.CapUnselect:    {},
		imaplib.CapLiteralPlus: {},
	}

	s.srv = imapserver.New(&imapserver.Options{
		NewSession:   s.newSession,
		Caps:         caps,
		TLSConfig:    opts.TLSConfig,
		InsecureAuth: opts.TLSConfig == nil,
		Logger:       &slogLogger{},
	})
	return s
}

// ListenAndServeTLS starts the TLS listener (port 993).
func (s *Server) ListenAndServeTLS() error {
	if s.opts.TLSConfig == nil {
		return fmt.Errorf("imap: TLS config required for IMAPS")
	}
	ln, err := tls.Listen("tcp", s.opts.Addr, s.opts.TLSConfig)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (TLS)", "addr", s.opts.Addr)
	return s.srv.Serve(ln)
}

// Serve accepts connections on the given listener.
func (s *Server) Serve(ln net.Listener) error {
	return s.srv.Serve(ln)
}

// ListenAndServe starts the plain STARTTLS listener (port 143).
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.AddrPlain)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (STARTTLS)", "addr", s.opts.AddrPlain)
	return s.srv.Serve(ln)
}

func (s *Server) newSession(_ *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	sess := &session{srv: s}
	return sess, &imapserver.GreetingData{PreAuth: false}, nil
}

// ---- session ------------------------------------------------------------

type session struct {
	srv      *Server
	username string
	folder   *mailbox.Folder
}

var _ imapserver.SessionIMAP4rev2 = (*session)(nil)

func (s *session) Close() error { return nil }

func (s *session) Login(username, password string) error {
	res, err := s.srv.opts.Auth.Authenticate(username, password, "imap")
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Invalid credentials"}
	}
	s.username = res.Username
	if err := s.srv.opts.Mailbox.Init(s.username); err != nil {
		slog.Error("imap: mailbox init failed", "user", s.username, "err", err)
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Internal error"}
	}
	return nil
}

func (s *session) Select(name string, _ *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	exists, err := s.srv.opts.Mailbox.FolderExists(s.username, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &imaplib.Error{
			Type: imaplib.StatusResponseTypeNo,
			Code: imaplib.ResponseCodeNonExistent,
			Text: "No such mailbox",
		}
	}
	f, err := s.srv.opts.Index.OpenFolder(s.username, name, uint32(time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	s.folder = f

	msgs, _ := s.srv.opts.Index.GetMessages(f.ID, mailbox.SeqSet{})
	return &imaplib.SelectData{
		Flags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
		},
		PermanentFlags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
			imaplib.Flag(`\*`), // supports user-defined keywords
		},
		NumMessages: uint32(len(msgs)),
		UIDValidity: f.UIDValidity,
		UIDNext:     imaplib.UID(f.NextUID),
	}, nil
}

func (s *session) Unselect() error {
	s.folder = nil
	return nil
}

func (s *session) Create(name string, _ *imaplib.CreateOptions) error {
	return s.srv.opts.Mailbox.Create(s.username, name)
}

func (s *session) Delete(name string) error {
	return s.srv.opts.Mailbox.Delete(s.username, name)
}

func (s *session) Rename(_, _ string, _ *imaplib.RenameOptions) error {
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "RENAME not yet implemented"}
}

func (s *session) Subscribe(_ string) error   { return nil }
func (s *session) Unsubscribe(_ string) error { return nil }

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, _ *imaplib.ListOptions) error {
	folders, err := s.srv.opts.Mailbox.ListFolders(s.username)
	if err != nil {
		return err
	}
	for _, name := range folders {
		full := ref + name
		if !listMatch(full, patterns) {
			continue
		}
		if err := w.WriteList(&imaplib.ListData{Mailbox: full, Delim: '/'}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	f, err := s.srv.opts.Index.OpenFolder(s.username, name, 0)
	if err != nil {
		return nil, err
	}
	msgs, _ := s.srv.opts.Index.GetMessages(f.ID, mailbox.SeqSet{})
	var unseen uint32
	for _, m := range msgs {
		if !hasFlag(m.Flags, `\Seen`) {
			unseen++
		}
	}
	d := &imaplib.StatusData{Mailbox: name}
	if opts.NumMessages {
		n := uint32(len(msgs))
		d.NumMessages = &n
	}
	if opts.UIDNext {
		d.UIDNext = imaplib.UID(f.NextUID)
	}
	if opts.UIDValidity {
		d.UIDValidity = f.UIDValidity
	}
	if opts.NumUnseen {
		d.NumUnseen = &unseen
	}
	if opts.HighestModSeq {
		d.HighestModSeq = f.HighestModSeq
	}
	return d, nil
}

func (s *session) Append(name string, r imaplib.LiteralReader, opts *imaplib.AppendOptions) (*imaplib.AppendData, error) {
	f, err := s.ensureFolder(name)
	if err != nil {
		return nil, err
	}

	var flagList, kwList []string
	if opts != nil {
		for _, fl := range opts.Flags {
			s := string(fl)
			if strings.HasPrefix(s, `\`) {
				flagList = append(flagList, s)
			} else {
				kwList = append(kwList, s)
			}
		}
	}

	size := r.Size()
	modseq, _ := s.srv.opts.Index.NextModSeq(f.ID)
	uid := f.NextUID
	f.NextUID++
	f.Messages++

	filename, err := s.srv.opts.Mailbox.Save(s.username, name, r, size, flagList)
	if err != nil {
		return nil, err
	}

	meta := &mailbox.MessageMeta{UID: uid, Filename: filename, Flags: flagList, Keywords: kwList, ModSeq: modseq, Size: uint32(size)}
	if err := s.srv.opts.Index.AppendMessage(f.ID, meta); err != nil {
		return nil, err
	}

	if mbe, ok := s.srv.opts.Mailbox.(interface {
		AppendUIDEntry(user, folder string, uid uint32, filename string) error
	}); ok {
		mbe.AppendUIDEntry(s.username, name, uid, filename) //nolint:errcheck
	}

	s.srv.opts.Index.SaveFolder(s.username, f) //nolint:errcheck

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(uid)}, nil
}

func (s *session) Poll(_ *imapserver.UpdateWriter, _ bool) error { return nil }

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.srv.opts.Index.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if !hasFlag(m.Flags, `\Deleted`) {
			continue
		}
		if uids != nil && !uids.Contains(imaplib.UID(m.UID)) {
			continue
		}
		s.srv.opts.Index.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		if err := w.WriteExpunge(m.UID); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Search(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, _ *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.srv.opts.Index.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, err
	}
	data := &imaplib.SearchData{}
	if kind == imapserver.NumKindUID {
		var result imaplib.UIDSet
		for _, m := range msgs {
			if matchesCriteria(m, criteria) {
				result.AddNum(imaplib.UID(m.UID))
			}
		}
		data.All = result
	} else {
		var result imaplib.SeqSet
		for i, m := range msgs {
			if matchesCriteria(m, criteria) {
				result.AddNum(uint32(i + 1))
			}
		}
		data.All = result
	}
	return data, nil
}

func (s *session) Fetch(w *imapserver.FetchWriter, numSet imaplib.NumSet, opts *imaplib.FetchOptions) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.srv.opts.Index.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		mw := w.CreateMessage(seqNum)
		if opts.Flags {
			mw.WriteFlags(toImapFlags(append(m.Flags, m.Keywords...)))
		}
		if opts.UID {
			mw.WriteUID(imaplib.UID(m.UID))
		}
		if opts.InternalDate {
			mw.WriteInternalDate(m.InternalDate)
		}
		if opts.RFC822Size {
			mw.WriteRFC822Size(int64(m.Size))
		}
		for _, section := range opts.BodySection {
			if m.Filename == "" {
				break
			}
			rc, ferr := s.srv.opts.Mailbox.Fetch(s.username, s.folder.Name, m.Filename)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			bw := mw.WriteBodySection(section, int64(len(body)))
			io.Copy(bw, bytes.NewReader(body)) //nolint:errcheck
			bw.Close()
		}
		mw.Close() //nolint:errcheck
	}
	return nil
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imaplib.NumSet, storeFlags *imaplib.StoreFlags, _ *imaplib.StoreOptions) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.srv.opts.Index.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		allNew := applyStoreFlags(append(m.Flags, m.Keywords...), storeFlags)
		var newFlags, newKW []string
		for _, f := range allNew {
			if strings.HasPrefix(f, `\`) {
				newFlags = append(newFlags, f)
			} else {
				newKW = append(newKW, f)
			}
		}
		s.srv.opts.Index.UpdateFlags(s.folder.ID, m.UID, newFlags, newKW) //nolint:errcheck

		if !storeFlags.Silent {
			mw := w.CreateMessage(seqNum)
			mw.WriteFlags(toImapFlags(append(newFlags, newKW...)))
			mw.WriteUID(imaplib.UID(m.UID))
			mw.Close() //nolint:errcheck
		}
	}
	return nil
}

func (s *session) Copy(numSet imaplib.NumSet, dest string) (*imaplib.CopyData, error) {
	return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "COPY not yet implemented"}
}

// Namespace satisfies SessionNamespace.
func (s *session) Namespace() (*imaplib.NamespaceData, error) {
	return &imaplib.NamespaceData{
		Personal: []imaplib.NamespaceDescriptor{{Delim: '/'}},
	}, nil
}

// Move satisfies SessionMove.
func (s *session) Move(_ *imapserver.MoveWriter, _ imaplib.NumSet, _ string) error {
	return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "MOVE not yet implemented"}
}

// ---- helpers ------------------------------------------------------------

type slogLogger struct{}

func (l *slogLogger) Printf(format string, args ...interface{}) {
	slog.Error(fmt.Sprintf(format, args...))
}

func (s *session) ensureFolder(name string) (*mailbox.Folder, error) {
	if s.folder != nil && s.folder.Name == name {
		return s.folder, nil
	}
	return s.srv.opts.Index.OpenFolder(s.username, name, uint32(time.Now().Unix()))
}

func listMatch(name string, patterns []string) bool {
	for _, p := range patterns {
		if p == "*" || p == "%" {
			return true
		}
		if strings.EqualFold(name, p) {
			return true
		}
	}
	return len(patterns) == 0
}

func hasFlag(flags []string, f string) bool {
	for _, fl := range flags {
		if fl == f {
			return true
		}
	}
	return false
}

func toImapFlags(flags []string) []imaplib.Flag {
	out := make([]imaplib.Flag, len(flags))
	for i, f := range flags {
		out[i] = imaplib.Flag(f)
	}
	return out
}

// numSetContains checks whether seqNum (for SeqSet) or uid (for UIDSet) is in numSet.
func numSetContains(numSet imaplib.NumSet, seqNum uint32, uid imaplib.UID) bool {
	switch ns := numSet.(type) {
	case imaplib.SeqSet:
		return ns.Contains(seqNum)
	case imaplib.UIDSet:
		return ns.Contains(uid)
	}
	return false
}

func matchesCriteria(m *mailbox.MessageMeta, criteria *imaplib.SearchCriteria) bool {
	if criteria == nil {
		return true
	}
	for _, f := range criteria.Flag {
		if !hasFlag(m.Flags, string(f)) {
			return false
		}
	}
	for _, f := range criteria.NotFlag {
		if hasFlag(m.Flags, string(f)) {
			return false
		}
	}
	return true
}

func applyStoreFlags(current []string, store *imaplib.StoreFlags) []string {
	newFlags := make([]string, len(store.Flags))
	for i, f := range store.Flags {
		newFlags[i] = string(f)
	}
	switch store.Op {
	case imaplib.StoreFlagsSet:
		return newFlags
	case imaplib.StoreFlagsAdd:
		result := make([]string, len(current))
		copy(result, current)
		for _, f := range newFlags {
			if !hasFlag(result, f) {
				result = append(result, f)
			}
		}
		return result
	case imaplib.StoreFlagsDel:
		var result []string
		for _, cf := range current {
			if !hasFlag(newFlags, cf) {
				result = append(result, cf)
			}
		}
		return result
	}
	return current
}
