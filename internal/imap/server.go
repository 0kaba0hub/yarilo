// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	"github.com/0kaba0hub/yarilo/pkg/mailbox"
)

// Server is the yarilo IMAP server.
type Server struct {
	srv  *imapserver.Server
	opts Options
}

// Options configures the IMAP server.
type Options struct {
	Addr               string
	AddrPlain          string
	TLSConfig          *tls.Config
	Mailbox            mailbox.MailboxBackend
	Index              mailbox.IndexBackend
	Resolver           *mailbox.Resolver
	Auth               protocol.Passdb
	ProxyProtocol      bool
	HAProxyTimeout     time.Duration
	HAProxyTrustedNets []*net.IPNet
	XClient            bool
	XClientTrustedNets []*net.IPNet
	DisablePlainAuth   bool
	IdleNotifyInterval time.Duration
	MaxLineLength      int
	ConnLimit          *connlimit.Limiter
	IDSend             string
	LoginGreeting      string
	LogoutFormat       string
	ClientWorkarounds  imapWorkarounds
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
		InsecureAuth: !opts.DisablePlainAuth,
		Logger:       &slogLogger{},
	})
	return s
}

func (s *Server) ListenAndServeTLS() error {
	if s.opts.TLSConfig == nil {
		return fmt.Errorf("imap: TLS config required for IMAPS")
	}
	ln, err := tls.Listen("tcp", s.opts.Addr, s.opts.TLSConfig)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (TLS)", "addr", s.opts.Addr)
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) Serve(ln net.Listener) error {
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.opts.AddrPlain)
	if err != nil {
		return err
	}
	slog.Info("imap: listening (STARTTLS)", "addr", s.opts.AddrPlain)
	return s.srv.Serve(s.wrapProxy(ln))
}

func (s *Server) wrapProxy(ln net.Listener) net.Listener {
	if s.opts.ProxyProtocol {
		timeout := s.opts.HAProxyTimeout
		if timeout == 0 {
			timeout = 3 * time.Second
		}
		ln = &proxyproto.Listener{
			Listener:          ln,
			ReadHeaderTimeout: timeout,
			Policy:            proxyPolicy(s.opts.HAProxyTrustedNets),
		}
	}
	if s.opts.MaxLineLength > 0 {
		ln = &maxLineLenListener{Listener: ln, limit: s.opts.MaxLineLength}
	}
	if s.opts.XClient {
		ln = &xclientImapListener{Listener: ln, trustedNets: s.opts.XClientTrustedNets}
	}
	if s.opts.LoginGreeting != "" {
		ln = &greetingListener{Listener: ln, greeting: s.opts.LoginGreeting}
	}
	if s.opts.IDSend != "" {
		ln = newIDListener(ln, s.opts.IDSend)
	}
	return ln
}

func proxyPolicy(nets []*net.IPNet) func(net.Addr) (proxyproto.Policy, error) {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		if len(nets) == 0 {
			return proxyproto.IGNORE, nil
		}
		tcp, ok := upstream.(*net.TCPAddr)
		if !ok {
			return proxyproto.IGNORE, nil
		}
		for _, n := range nets {
			if n.Contains(tcp.IP) {
				return proxyproto.USE, nil
			}
		}
		return proxyproto.IGNORE, nil
	}
}

func (s *Server) newSession(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	sess := &session{srv: s, imapConn: c}
	return sess, &imapserver.GreetingData{PreAuth: false}, nil
}

// ---- session ---------------------------------------------------------------

type session struct {
	srv      *Server
	imapConn *imapserver.Conn
	userInfo *mailbox.UserInfo
	box      mailbox.UserMailbox
	idx      mailbox.UserIndex
	limitIP  string
	folder   *mailbox.Folder

	statsDeleted    int
	statsExpunged   int
	statsFetchHdr   int
	statsFetchHdrB  int64
	statsFetchBody  int
	statsFetchBodyB int64
}

var _ imapserver.SessionIMAP4rev2 = (*session)(nil)

func (s *session) Close() error {
	if s.srv.opts.ConnLimit != nil && s.userInfo != nil {
		s.srv.opts.ConnLimit.Release(s.userInfo.Username, s.limitIP)
	}
	if s.srv.opts.LogoutFormat != "" && s.userInfo != nil {
		msg := formatLogoutMsg(s.srv.opts.LogoutFormat, map[string]string{
			"deleted":          strconv.Itoa(s.statsDeleted),
			"expunged":         strconv.Itoa(s.statsExpunged),
			"fetch_hdr_count":  strconv.Itoa(s.statsFetchHdr),
			"fetch_hdr_bytes":  strconv.FormatInt(s.statsFetchHdrB, 10),
			"fetch_body_count": strconv.Itoa(s.statsFetchBody),
			"fetch_body_bytes": strconv.FormatInt(s.statsFetchBodyB, 10),
		})
		slog.Info("imap: logout", "user", s.userInfo.Username, "stats", msg)
	}
	if s.box != nil {
		s.box.Close() //nolint:errcheck
	}
	if s.idx != nil {
		s.idx.Close() //nolint:errcheck
	}
	return nil
}

func formatLogoutMsg(format string, vars map[string]string) string {
	var sb strings.Builder
	i := 0
	for i < len(format) {
		if format[i] != '%' || i+2 >= len(format) || format[i+1] != '{' {
			sb.WriteByte(format[i])
			i++
			continue
		}
		end := strings.IndexByte(format[i:], '}')
		if end < 0 {
			sb.WriteByte(format[i])
			i++
			continue
		}
		key := format[i+2 : i+end]
		if v, ok := vars[key]; ok {
			sb.WriteString(v)
		} else {
			sb.WriteString(format[i : i+end+1])
		}
		i += end + 1
	}
	return sb.String()
}

func (s *session) Login(username, password string) error {
	res, err := s.srv.opts.Auth.Authenticate(username, password, "imap")
	if err != nil || res == nil || res.Result != protocol.AuthOK {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Invalid credentials"}
	}

	resolver := s.srv.opts.Resolver
	if resolver == nil {
		resolver = &mailbox.Resolver{}
	}
	userInfo := resolver.UserInfo(res.Username, res.Home)

	if lim := s.srv.opts.ConnLimit; lim != nil {
		ip := remoteIP(s.imapConn.NetConn())
		if !lim.Acquire(userInfo.Username, ip) {
			slog.Warn("imap: connection limit reached", "user", userInfo.Username, "ip", ip)
			return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Too many simultaneous connections"}
		}
		s.limitIP = ip
	}

	box := s.srv.opts.Mailbox.OpenUser(userInfo)
	if err := box.Init(); err != nil {
		slog.Error("imap: mailbox init failed", "user", userInfo.Username, "err", err)
		if s.srv.opts.ConnLimit != nil {
			s.srv.opts.ConnLimit.Release(userInfo.Username, s.limitIP)
		}
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Internal error"}
	}

	s.userInfo = userInfo
	s.box = box
	s.idx = s.srv.opts.Index.OpenUser(userInfo)
	return nil
}

func remoteIP(c net.Conn) string {
	if c == nil {
		return ""
	}
	if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		return tcp.IP.String()
	}
	return c.RemoteAddr().String()
}

func (s *session) Select(name string, _ *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	exists, err := s.box.FolderExists(name)
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
	f, err := s.idx.OpenFolder(name, uint32(time.Now().Unix()))
	if err != nil {
		return nil, err
	}
	s.folder = f

	msgs, _ := s.idx.GetMessages(f.ID, mailbox.SeqSet{})
	return &imaplib.SelectData{
		Flags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
		},
		PermanentFlags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
			imaplib.Flag(`\*`),
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
	return s.box.Create(name)
}

func (s *session) Delete(name string) error {
	return s.box.Delete(name)
}

func (s *session) Rename(oldName, newName string, _ *imaplib.RenameOptions) error {
	if strings.EqualFold(oldName, "INBOX") {
		return s.renameInbox(newName)
	}
	if err := s.box.Rename(oldName, newName); err != nil {
		return err
	}
	return s.idx.RenameFolder(oldName, newName)
}

// renameInbox implements RFC 3501 §6.3.5 INBOX rename semantics:
// messages are moved to the new mailbox; INBOX itself is cleared but not deleted.
func (s *session) renameInbox(dest string) error {
	if err := s.box.Create(dest); err != nil {
		return fmt.Errorf("imap/rename-inbox create: %w", err)
	}
	srcFolder, err := s.idx.OpenFolder("INBOX", 0)
	if err != nil {
		return err
	}
	msgs, err := s.idx.GetMessages(srcFolder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	destFolder, err := s.idx.OpenFolder(dest, uint32(time.Now().Unix()))
	if err != nil {
		return err
	}
	for _, m := range msgs {
		rc, fetchErr := s.box.Fetch("INBOX", m.Filename)
		if fetchErr != nil {
			return fmt.Errorf("imap/rename-inbox fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/rename-inbox read: %w", readErr)
		}
		modseq, _ := s.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := s.box.Save(dest, bytes.NewReader(data), int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/rename-inbox save: %w", saveErr)
		}
		meta := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		newUID, appendErr := s.idx.AllocateAppend(destFolder.ID, meta)
		if appendErr != nil {
			return fmt.Errorf("imap/rename-inbox append: %w", appendErr)
		}
		s.box.AppendUIDEntry(dest, newUID, newFilename) //nolint:errcheck
		s.box.Remove("INBOX", m.Filename)               //nolint:errcheck
		s.idx.ExpungeMessage(srcFolder.ID, m.UID)       //nolint:errcheck
	}
	srcFolder.Messages = 0
	s.idx.SaveFolder(srcFolder) //nolint:errcheck
	return nil
}

func (s *session) Subscribe(_ string) error   { return nil }
func (s *session) Unsubscribe(_ string) error { return nil }

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, _ *imaplib.ListOptions) error {
	if s.srv.opts.ClientWorkarounds&workaroundTBExtraMailboxSep != 0 {
		ref = strings.TrimPrefix(ref, "/")
		for i, p := range patterns {
			patterns[i] = strings.TrimPrefix(p, "/")
		}
	}
	folders, err := s.box.ListFolders()
	if err != nil {
		return err
	}
	for _, name := range folders {
		full := ref + name
		if !listMatch(full, patterns) {
			continue
		}
		attrs := mailboxAttrs(name, folders, s.srv.opts.ClientWorkarounds)
		if err := w.WriteList(&imaplib.ListData{Mailbox: full, Delim: '/', Attrs: attrs}); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	f, err := s.idx.OpenFolder(name, 0)
	if err != nil {
		return nil, err
	}
	msgs, _ := s.idx.GetMessages(f.ID, mailbox.SeqSet{})
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
			fs := string(fl)
			if strings.HasPrefix(fs, `\`) {
				flagList = append(flagList, fs)
			} else {
				kwList = append(kwList, fs)
			}
		}
	}

	size := r.Size()
	modseq, _ := s.idx.NextModSeq(f.ID)

	filename, err := s.box.Save(name, r, size, flagList)
	if err != nil {
		return nil, err
	}

	meta := &mailbox.MessageMeta{Filename: filename, Flags: flagList, Keywords: kwList, ModSeq: modseq, Size: uint32(size)}
	uid, err := s.idx.AllocateAppend(f.ID, meta)
	if err != nil {
		return nil, err
	}

	s.box.AppendUIDEntry(name, uid, filename) //nolint:errcheck

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(uid)}, nil
}

func (s *session) Poll(_ *imapserver.UpdateWriter, _ bool) error { return nil }

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	interval := s.srv.opts.IdleNotifyInterval
	if interval <= 0 || s.folder == nil {
		<-stop
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if err := w.WriteNumMessages(s.folder.Messages); err != nil {
				return err
			}
		}
	}
}

func (s *session) Expunge(w *imapserver.ExpungeWriter, uids *imaplib.UIDSet) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
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
		s.idx.ExpungeMessage(s.folder.ID, m.UID) //nolint:errcheck
		s.statsExpunged++
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
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
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
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
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
			rc, ferr := s.box.Fetch(s.folder.Name, m.Filename)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			switch section.Specifier {
			case imaplib.PartSpecifierHeader, imaplib.PartSpecifierMIME:
				s.statsFetchHdr++
				s.statsFetchHdrB += int64(len(body))
			default:
				s.statsFetchBody++
				s.statsFetchBodyB += int64(len(body))
			}
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
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		current := append(m.Flags, m.Keywords...)
		wasDeleted := hasFlag(current, `\Deleted`)
		allNew := applyStoreFlags(current, storeFlags)
		if !wasDeleted && hasFlag(allNew, `\Deleted`) {
			s.statsDeleted++
		}
		var newFlags, newKW []string
		for _, f := range allNew {
			if strings.HasPrefix(f, `\`) {
				newFlags = append(newFlags, f)
			} else {
				newKW = append(newKW, f)
			}
		}
		s.idx.UpdateFlags(s.folder.ID, m.UID, newFlags, newKW) //nolint:errcheck

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
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	exists, err := s.box.FolderExists(dest)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, err
	}
	destFolder, err := s.ensureFolder(dest)
	if err != nil {
		return nil, err
	}
	var srcUIDs, dstUIDs imaplib.UIDSet
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := s.box.Fetch(s.folder.Name, m.Filename)
		if fetchErr != nil {
			return nil, fmt.Errorf("imap/copy fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return nil, fmt.Errorf("imap/copy read: %w", readErr)
		}
		modseq, _ := s.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := s.box.Save(dest, bytes.NewReader(data), int64(len(data)), m.Flags)
		if saveErr != nil {
			return nil, fmt.Errorf("imap/copy save: %w", saveErr)
		}
		meta := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		newUID, appendErr := s.idx.AllocateAppend(destFolder.ID, meta)
		if appendErr != nil {
			return nil, fmt.Errorf("imap/copy append: %w", appendErr)
		}
		s.box.AppendUIDEntry(dest, newUID, newFilename) //nolint:errcheck
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(newUID))
	}
	return &imaplib.CopyData{
		UIDValidity: destFolder.UIDValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    dstUIDs,
	}, nil
}

func (s *session) Namespace() (*imaplib.NamespaceData, error) {
	return &imaplib.NamespaceData{
		Personal: []imaplib.NamespaceDescriptor{{Delim: '/'}},
	}, nil
}

func (s *session) Move(w *imapserver.MoveWriter, numSet imaplib.NumSet, dest string) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	exists, err := s.box.FolderExists(dest)
	if err != nil {
		return err
	}
	if !exists {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeTryCreate, Text: "No such mailbox"}
	}
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}
	destFolder, err := s.ensureFolder(dest)
	if err != nil {
		return err
	}

	type matched struct {
		seqNum   uint32
		srcUID   uint32
		filename string
	}
	var hits []matched
	var srcUIDs, dstUIDs imaplib.UIDSet

	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		rc, fetchErr := s.box.Fetch(s.folder.Name, m.Filename)
		if fetchErr != nil {
			return fmt.Errorf("imap/move fetch: %w", fetchErr)
		}
		data, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			return fmt.Errorf("imap/move read: %w", readErr)
		}
		modseq, _ := s.idx.NextModSeq(destFolder.ID)
		newFilename, saveErr := s.box.Save(dest, bytes.NewReader(data), int64(len(data)), m.Flags)
		if saveErr != nil {
			return fmt.Errorf("imap/move save: %w", saveErr)
		}
		meta := &mailbox.MessageMeta{
			Filename:     newFilename,
			Flags:        m.Flags,
			Keywords:     m.Keywords,
			ModSeq:       modseq,
			Size:         uint32(len(data)),
			InternalDate: m.InternalDate,
		}
		newUID, appendErr := s.idx.AllocateAppend(destFolder.ID, meta)
		if appendErr != nil {
			return fmt.Errorf("imap/move append: %w", appendErr)
		}
		s.box.AppendUIDEntry(dest, newUID, newFilename) //nolint:errcheck
		srcUIDs.AddNum(imaplib.UID(m.UID))
		dstUIDs.AddNum(imaplib.UID(newUID))
		hits = append(hits, matched{seqNum: seqNum, srcUID: m.UID, filename: m.Filename})
	}

	if err := w.WriteCopyData(&imaplib.CopyData{
		UIDValidity: destFolder.UIDValidity,
		SourceUIDs:  srcUIDs,
		DestUIDs:    dstUIDs,
	}); err != nil {
		return err
	}

	// Expunge source in descending seq order (RFC 6851 §3.3).
	for i := len(hits) - 1; i >= 0; i-- {
		h := hits[i]
		s.box.Remove(s.folder.Name, h.filename)     //nolint:errcheck
		s.idx.ExpungeMessage(s.folder.ID, h.srcUID) //nolint:errcheck
		if err := w.WriteExpunge(h.seqNum); err != nil {
			return err
		}
	}
	s.folder.Messages -= uint32(len(hits))
	s.idx.SaveFolder(s.folder) //nolint:errcheck
	return nil
}

// ---- helpers ---------------------------------------------------------------

type slogLogger struct{}

func (l *slogLogger) Printf(format string, args ...interface{}) {
	slog.Error(fmt.Sprintf(format, args...))
}

func (s *session) ensureFolder(name string) (*mailbox.Folder, error) {
	if s.folder != nil && s.folder.Name == name {
		return s.folder, nil
	}
	return s.idx.OpenFolder(name, uint32(time.Now().Unix()))
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
