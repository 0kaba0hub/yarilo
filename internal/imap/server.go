// Package imap wires go-imap/v2 to yarilo's mailbox and index backends.
package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	proxyproto "github.com/pires/go-proxyproto"

	"github.com/0kaba0hub/yarilo/internal/auth/protocol"
	"github.com/0kaba0hub/yarilo/internal/connlimit"
	"github.com/0kaba0hub/yarilo/pkg/locks"
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

	// Locker is the cross-process write coordinator. When non-nil, each
	// successful write (APPEND/COPY/MOVE/STORE/EXPUNGE) emits an EVENT on
	// the mailbox key so IMAP IDLE sessions on other pods are woken up
	// without waiting for the heartbeat interval. Nil keeps the legacy
	// timer-based IDLE behaviour.
	Locker locks.Locker
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
		// IMAP4rev2 (RFC 9051) requires these. Some are wire-level (ENABLE,
		// SASL-IR) — declaring them is enough; go-imap/v2 handles the
		// protocol mechanics. The rest (ESEARCH, SEARCHRES, STATUS=SIZE)
		// require semantic implementation in Search/Status below.
		imaplib.CapESearch:      {},
		imaplib.CapSearchRes:    {},
		imaplib.CapEnable:       {},
		imaplib.CapSASLIR:       {},
		imaplib.CapStatusSize:   {},
		imaplib.CapListExtended: {},
		imaplib.CapListStatus:   {},
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

	// savedSearchUIDs holds the most recent SEARCH result that was issued
	// with RETURN SAVE (RFC 5182). Subsequent commands that reference $ get
	// this set substituted in via go-imap/v2's IsSearchRes detection.
	savedSearchUIDs imaplib.UIDSet

	// subs persists the SUBSCRIBE/UNSUBSCRIBE state (RFC 9051 + RFC 5258).
	// Constructed lazily after authentication.
	subs *subscriptionStore

	statsDeleted    int
	statsExpunged   int
	statsFetchHdr   int
	statsFetchHdrB  int64
	statsFetchBody  int
	statsFetchBodyB int64
}

var _ imapserver.SessionIMAP4rev2 = (*session)(nil)

// emitMailboxChange is fire-and-forget — events are advisory wake-ups for
// subscribed IMAP IDLE sessions on other pods. Errors are logged at debug
// level and never surfaced to the caller because the authoritative state
// already lives in the just-written index/uidlist files. A 1-second timeout
// keeps a sluggish locks server from stalling the IMAP command.
func (s *session) emitMailboxChange(folder string, eventType locks.EventType, uid uint32) {
	if s.srv.opts.Locker == nil || s.userInfo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	payload := strconv.FormatUint(uint64(uid), 10)
	if err := s.srv.opts.Locker.Emit(ctx, locks.MailboxKey(s.userInfo.Username, folder), eventType, payload); err != nil {
		slog.Debug("imap: emit event failed",
			"folder", folder, "type", string(eventType), "err", err)
	}
}

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
	s.subs = newSubscriptionStore(
		userInfo.Home,
		userInfo.Username,
		fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), userInfo.Username),
		s.srv.opts.Locker,
	)
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
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
		s.box.Remove("INBOX", m.Filename)         //nolint:errcheck
		s.idx.ExpungeMessage(srcFolder.ID, m.UID) //nolint:errcheck
		s.emitMailboxChange("INBOX", locks.EventExpunged, m.UID)
	}
	srcFolder.Messages = 0
	s.idx.SaveFolder(srcFolder) //nolint:errcheck
	return nil
}

func (s *session) Subscribe(name string) error {
	if s.subs == nil {
		return nil
	}
	if err := s.subs.Add(name); err != nil {
		return fmt.Errorf("imap: subscribe %q: %w", name, err)
	}
	return nil
}

func (s *session) Unsubscribe(name string) error {
	if s.subs == nil {
		return nil
	}
	if err := s.subs.Remove(name); err != nil {
		return fmt.Errorf("imap: unsubscribe %q: %w", name, err)
	}
	return nil
}

func (s *session) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imaplib.ListOptions) error {
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

	// Snapshot subscriptions once per LIST — every folder's ReturnSubscribed
	// / SelectSubscribed decision consults the same view, even if a sibling
	// session SUBSCRIBE'd mid-iteration.
	var subs map[string]struct{}
	if s.subs != nil && (opts != nil && (opts.SelectSubscribed || opts.ReturnSubscribed)) {
		subs, err = s.subs.Snapshot()
		if err != nil {
			slog.Warn("imap: subscription snapshot failed", "err", err)
			subs = make(map[string]struct{})
		}
	}

	for _, name := range folders {
		full := ref + name
		if !listMatch(full, patterns) {
			continue
		}
		// SELECT SUBSCRIBED — drop folders the user has not subscribed to.
		// RECURSIVEMATCH refinement (return parent even if only a child is
		// subscribed) is out of scope for IMAP-B; clients that need it must
		// LIST without the filter and post-filter on their side.
		if opts != nil && opts.SelectSubscribed {
			if _, ok := subs[name]; !ok {
				continue
			}
		}
		attrs := mailboxAttrs(name, folders, s.srv.opts.ClientWorkarounds)
		// RETURN CHILDREN — annotate \HasChildren / \HasNoChildren.
		if opts != nil && opts.ReturnChildren {
			attrs = append(attrs, childrenAttr(name, folders))
		}
		// RETURN SUBSCRIBED — annotate \Subscribed when applicable.
		if opts != nil && opts.ReturnSubscribed {
			if _, ok := subs[name]; ok {
				attrs = append(attrs, imaplib.MailboxAttrSubscribed)
			}
		}
		data := &imaplib.ListData{Mailbox: full, Delim: '/', Attrs: attrs}
		// RETURN STATUS (RFC 5819 / IMAP4rev2) — per-folder Status response
		// embedded in the LIST reply. Skip on failure rather than abort the
		// whole LIST.
		if opts != nil && opts.ReturnStatus != nil {
			if status, statErr := s.Status(name, opts.ReturnStatus); statErr == nil {
				data.Status = status
			}
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// childrenAttr returns \HasChildren when `name` is a prefix of any other
// listed folder, otherwise \HasNoChildren. Cheap O(n) scan — acceptable
// because LIST already loaded the slice.
func childrenAttr(name string, all []string) imaplib.MailboxAttr {
	prefix := name + "/"
	for _, other := range all {
		if other == name {
			continue
		}
		if strings.HasPrefix(other, prefix) {
			return imaplib.MailboxAttrHasChildren
		}
	}
	return imaplib.MailboxAttrHasNoChildren
}

func (s *session) Status(name string, opts *imaplib.StatusOptions) (*imaplib.StatusData, error) {
	f, err := s.idx.OpenFolder(name, 0)
	if err != nil {
		return nil, err
	}
	msgs, _ := s.idx.GetMessages(f.ID, mailbox.SeqSet{})
	var (
		unseen    uint32
		deleted   uint32
		totalSize int64
	)
	for _, m := range msgs {
		if !hasFlag(m.Flags, `\Seen`) {
			unseen++
		}
		if hasFlag(m.Flags, `\Deleted`) {
			deleted++
		}
	}
	// STATUS=SIZE (RFC 8438, also IMAP4rev2 required) — the FileIndex
	// record does not carry message size; pull it from the maildir/dbox
	// filename via box.List which extracts the ",S=<phys>" suffix.
	// Only walked when the client asked for SIZE so the common STATUS
	// path stays cheap.
	if opts.Size {
		boxMsgs, listErr := s.box.List(name)
		if listErr == nil {
			for _, bm := range boxMsgs {
				totalSize += int64(bm.Size)
			}
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
	if opts.NumDeleted {
		d.NumDeleted = &deleted
	}
	if opts.Size {
		d.Size = &totalSize
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
	s.emitMailboxChange(name, locks.EventDelivered, uid)

	return &imaplib.AppendData{UIDValidity: f.UIDValidity, UID: imaplib.UID(uid)}, nil
}

func (s *session) Poll(_ *imapserver.UpdateWriter, _ bool) error { return nil }

func (s *session) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	interval := s.srv.opts.IdleNotifyInterval
	if s.folder == nil {
		<-stop
		return nil
	}

	// Cross-pod event subscription: when another process (LMTP delivery, an
	// IMAP session on a sibling pod, etc.) writes to this user's folder, we
	// get an EVENT, refresh the message count from disk, and push the
	// EXISTS notification immediately — no waiting for the timer.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var events <-chan locks.Event
	if s.srv.opts.Locker != nil && s.userInfo != nil {
		ch, err := s.srv.opts.Locker.Subscribe(ctx, locks.MailboxKey(s.userInfo.Username, s.folder.Name))
		if err != nil {
			slog.Debug("imap: idle subscribe failed; falling back to timer-only", "err", err)
		} else {
			events = ch
		}
	}

	// Heartbeat tick — required only when there is no event channel; the
	// timer is still useful as a liveness signal for misbehaving clients
	// even when the subscription is up.
	var tickC <-chan time.Time
	if interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tickC = ticker.C
	}

	if events == nil && tickC == nil {
		// Locker disabled and no heartbeat configured — purely passive IDLE.
		<-stop
		return nil
	}

	for {
		select {
		case <-stop:
			return nil
		case _, ok := <-events:
			if !ok {
				events = nil // subscription dropped; keep heartbeat going
				continue
			}
			if err := s.refreshIdleCount(w); err != nil {
				return err
			}
		case <-tickC:
			if err := w.WriteNumMessages(s.folder.Messages); err != nil {
				return err
			}
		}
	}
}

// refreshIdleCount re-reads the selected folder's message count from the
// index and writes EXISTS. Used by IDLE after a cross-pod EVENT — the
// in-memory s.folder.Messages may be stale if another process appended.
func (s *session) refreshIdleCount(w *imapserver.UpdateWriter) error {
	if s.folder == nil {
		return nil
	}
	refreshed, err := s.idx.OpenFolder(s.folder.Name, s.folder.UIDValidity)
	if err != nil {
		// Best-effort: report what we have. Authoritative state lives on disk
		// and the next user command will re-read it.
		return w.WriteNumMessages(s.folder.Messages)
	}
	s.folder.Messages = refreshed.Messages
	return w.WriteNumMessages(refreshed.Messages)
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
		s.emitMailboxChange(s.folder.Name, locks.EventExpunged, m.UID)
		s.statsExpunged++
		if err := w.WriteExpunge(m.UID); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) Search(kind imapserver.NumKind, criteria *imaplib.SearchCriteria, opts *imaplib.SearchOptions) (*imaplib.SearchData, error) {
	if s.folder == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}

	// SearchRes ($) substitution: when the client passes "$" as a UID set,
	// go-imap/v2 surfaces it as an imaplib.SearchRes()-tagged entry in
	// criteria.UID. We swap it for the saved set from the previous RETURN
	// SAVE so the matcher sees a concrete UID list.
	criteria = s.substituteSearchRes(criteria)

	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return nil, err
	}

	// Collect both representations — clients may want UID set OR sequence
	// numbers via RETURN ALL, while MIN/MAX/COUNT always operate on the
	// kind requested.
	var (
		uidHits  imaplib.UIDSet
		seqHits  imaplib.SeqSet
		first    uint32
		last     uint32
		hitCount uint32
	)
	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !matchesCriteriaSeq(m, seqNum, criteria) {
			continue
		}
		hitCount++
		var current uint32
		if kind == imapserver.NumKindUID {
			current = m.UID
			uidHits.AddNum(imaplib.UID(m.UID))
		} else {
			current = seqNum
			seqHits.AddNum(seqNum)
		}
		if first == 0 || current < first {
			first = current
		}
		if current > last {
			last = current
		}
	}

	data := &imaplib.SearchData{}
	// ESEARCH RETURN handling. Per RFC 4731: when RETURN is given, only the
	// requested data items are sent. When RETURN is omitted, send ALL by
	// default (legacy SEARCH response).
	wantAll := opts == nil ||
		(!opts.ReturnMin && !opts.ReturnMax && !opts.ReturnCount && !opts.ReturnAll && !opts.ReturnSave)
	if wantAll || (opts != nil && opts.ReturnAll) {
		if kind == imapserver.NumKindUID {
			data.All = uidHits
		} else {
			data.All = seqHits
		}
	}
	if opts != nil && opts.ReturnMin {
		data.Min = first
	}
	if opts != nil && opts.ReturnMax {
		data.Max = last
	}
	if opts != nil && opts.ReturnCount {
		data.Count = hitCount
	}
	// SEARCHRES (RFC 5182): RETURN SAVE pins the hit set for later $ refs.
	// The spec says the saved set is always the UID-typed result; convert
	// from sequence numbers if SEARCH was issued in sequence-number mode.
	if opts != nil && opts.ReturnSave {
		if kind == imapserver.NumKindUID {
			s.savedSearchUIDs = uidHits
		} else {
			// Sequence-number SEARCH still saves UIDs (RFC 5182 §2.1).
			var saved imaplib.UIDSet
			for i, m := range msgs {
				if matchesCriteriaSeq(m, uint32(i+1), criteria) {
					saved.AddNum(imaplib.UID(m.UID))
				}
			}
			s.savedSearchUIDs = saved
		}
	}
	return data, nil
}

// substituteSearchRes walks criteria.UID looking for the SearchRes ($)
// marker and replaces it with the in-memory saved set from a previous
// RETURN SAVE. Returns the original criteria unchanged if no marker is
// present, so non-$ SEARCH calls pay no cost.
func (s *session) substituteSearchRes(criteria *imaplib.SearchCriteria) *imaplib.SearchCriteria {
	if criteria == nil {
		return criteria
	}
	needsSub := false
	for _, u := range criteria.UID {
		if imaplib.IsSearchRes(u) {
			needsSub = true
			break
		}
	}
	if !needsSub {
		return criteria
	}
	clone := *criteria
	clone.UID = make([]imaplib.UIDSet, 0, len(criteria.UID))
	for _, u := range criteria.UID {
		if imaplib.IsSearchRes(u) {
			clone.UID = append(clone.UID, s.savedSearchUIDs)
			continue
		}
		clone.UID = append(clone.UID, u)
	}
	return &clone
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
		s.emitMailboxChange(s.folder.Name, locks.EventChanged, m.UID)

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
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
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
		s.emitMailboxChange(dest, locks.EventDelivered, newUID)
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
		s.emitMailboxChange(s.folder.Name, locks.EventExpunged, h.srcUID)
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

// matchesCriteriaSeq evaluates a SearchCriteria against a single message.
// seqNum supplies the message's 1-based sequence number; 0 means "unknown"
// and any criteria.SeqNum filter then short-circuits to not-matched.
func matchesCriteriaSeq(m *mailbox.MessageMeta, seqNum uint32, criteria *imaplib.SearchCriteria) bool {
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
	// Each entry in criteria.UID / criteria.SeqNum is an AND condition (the
	// message must fall within EVERY listed set). Per RFC 9051 §6.4.4.
	for _, set := range criteria.UID {
		if !set.Contains(imaplib.UID(m.UID)) {
			return false
		}
	}
	for _, set := range criteria.SeqNum {
		if seqNum == 0 || !set.Contains(seqNum) {
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
