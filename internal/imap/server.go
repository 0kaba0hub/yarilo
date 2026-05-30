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
	"github.com/0kaba0hub/yarilo/pkg/dict"
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

	// SpecialUseDefaults is the folder-name → \Sent/\Drafts/etc. mapping
	// applied by LIST when the on-disk per-user special_use file does not
	// override. Driven by protocol.imap.imap_special_use_defaults in
	// yarilo.yaml.
	SpecialUseDefaults map[string]string

	// MetadataDict backs RFC 5464 METADATA (GETMETADATA / SETMETADATA).
	// When nil, the server still advertises METADATA / METADATA-SERVER
	// (the lib needs the caps to parse the commands) but every op
	// returns "metadata storage disabled". Operators wire this from
	// cfg.Dicts["metadata"] in yarilo.yaml.
	MetadataDict dict.Dict
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
		imaplib.CapESearch:          {},
		imaplib.CapSearchRes:        {},
		imaplib.CapEnable:           {},
		imaplib.CapSASLIR:           {},
		imaplib.CapStatusSize:       {},
		imaplib.CapListExtended:     {},
		imaplib.CapListStatus:       {},
		imaplib.CapSpecialUse:       {},
		imaplib.CapCreateSpecialUse: {},
		imaplib.CapBinary:           {},
		imaplib.CapQResync:          {},
		imaplib.CapMetadata:         {},
		// CapMetadataServer not in opts.Caps: the fork's capability
		// allow-list does not echo it back, but SessionMetadata's
		// mailbox=="" path handles server-scope ops anyway, so the
		// behaviour is preserved without the wire-level cap atom.
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

	// specialUse persists per-user RFC 6154 overrides set via CREATE
	// (USE ...) and resolves folder→attr for LIST.
	specialUse *specialUseStore

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
	owner := fmt.Sprintf("yarilo-imap/%d/%s", os.Getpid(), userInfo.Username)
	s.subs = newSubscriptionStore(userInfo.Home, userInfo.Username, owner, s.srv.opts.Locker)
	s.specialUse = newSpecialUseStore(
		userInfo.Home, userInfo.Username, owner, s.srv.opts.Locker,
		s.srv.opts.SpecialUseDefaults,
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

func (s *session) Select(name string, opts *imaplib.SelectOptions) (*imaplib.SelectData, error) {
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
	data := &imaplib.SelectData{
		Flags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
		},
		PermanentFlags: []imaplib.Flag{
			imaplib.FlagAnswered, imaplib.FlagFlagged,
			imaplib.FlagDeleted, imaplib.FlagSeen, imaplib.FlagDraft,
			imaplib.Flag(`\*`),
		},
		NumMessages:   uint32(len(msgs)),
		UIDValidity:   f.UIDValidity,
		UIDNext:       imaplib.UID(f.NextUID),
		HighestModSeq: f.HighestModSeq,
	}
	// QRESYNC SELECT (RFC 7162 §3.2): when the client supplies (UIDVALIDITY
	// <v> <last-known-modseq> [<known-uids>]) and the UIDVALIDITY matches,
	// reply with VANISHED (EARLIER) listing UIDs expunged since the client's
	// modseq. The KnownUIDs filter narrows the response to UIDs the client
	// actually remembers; an empty set means "tell me everything".
	if opts != nil && opts.QResync != nil && opts.QResync.UIDValidity == f.UIDValidity {
		vanishedUIDs, vErr := s.idx.Vanished(f.ID, opts.QResync.ModSeq)
		if vErr == nil && len(vanishedUIDs) > 0 {
			var vset imaplib.UIDSet
			if len(opts.QResync.KnownUIDs) == 0 {
				for _, uid := range vanishedUIDs {
					vset.AddNum(imaplib.UID(uid))
				}
			} else {
				for _, uid := range vanishedUIDs {
					if opts.QResync.KnownUIDs.Contains(imaplib.UID(uid)) {
						vset.AddNum(imaplib.UID(uid))
					}
				}
			}
			if len(vset) > 0 {
				data.Vanished = vset
			}
		}
	}
	return data, nil
}

func (s *session) Unselect() error {
	s.folder = nil
	return nil
}

func (s *session) Create(name string, opts *imaplib.CreateOptions) error {
	if err := s.box.Create(name); err != nil {
		return err
	}
	// CREATE-SPECIAL-USE (RFC 6154 §3): record the requested use attr so
	// subsequent LIST replies advertise it. RFC permits multiple USE attrs
	// in the request but forbids carrying more than one on the folder; we
	// honour the first one in the supplied slice and ignore the rest.
	if opts != nil && len(opts.SpecialUse) > 0 && s.specialUse != nil {
		if err := s.specialUse.Set(name, opts.SpecialUse[0]); err != nil {
			slog.Warn("imap: special_use persist failed",
				"folder", name, "attr", string(opts.SpecialUse[0]), "err", err)
		}
	}
	return nil
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
		// SPECIAL-USE (RFC 6154). The attribute is unconditional metadata
		// — clients that did not request RETURN SPECIAL-USE still expect
		// to see \Sent etc. when the folder qualifies. Dovecot advertises
		// the same way.
		if s.specialUse != nil {
			if attr := s.specialUse.Get(name); attr != "" {
				attrs = append(attrs, attr)
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
		uidHits    imaplib.UIDSet
		seqHits    imaplib.SeqSet
		first      uint32
		last       uint32
		hitCount   uint32
		highestMod uint64 // CONDSTORE MODSEQ across all matched messages
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
		if m.ModSeq > highestMod {
			highestMod = m.ModSeq
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
	// CONDSTORE — RFC 7162 §3.1.5. When any matched message carries a
	// modseq, surface the maximum so the client can persist its
	// "highest-seen modseq" for the folder. Emitted regardless of which
	// RETURN items were requested (the spec considers MODSEQ implicit
	// when SEARCH MODSEQ criteria are used, but it is also useful for
	// non-modseq searches against a CONDSTORE-enabled mailbox).
	if highestMod > 0 {
		data.ModSeq = highestMod
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
	// UID FETCH (CHANGEDSINCE N VANISHED) — RFC 7162 §3.2.10. Emit
	// VANISHED (EARLIER) for UIDs expunged since the supplied modseq.
	// VANISHED on a sequence-number FETCH is invalid; the patched lib
	// rejects that at parse time so we do not need to re-check here.
	if opts.Vanished && opts.ChangedSince > 0 {
		vanishedUIDs, vErr := s.idx.Vanished(s.folder.ID, opts.ChangedSince)
		if vErr == nil && len(vanishedUIDs) > 0 {
			var vset imaplib.UIDSet
			for _, uid := range vanishedUIDs {
				vset.AddNum(imaplib.UID(uid))
			}
			if writeErr := w.WriteVanished(vset); writeErr != nil {
				return writeErr
			}
		}
	}
	for i, m := range msgs {
		// CHANGEDSINCE filter — skip messages whose modseq has not moved
		// past the client's last-known value (RFC 4551 §3.3.1).
		if opts.ChangedSince > 0 && m.ModSeq <= opts.ChangedSince {
			continue
		}
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
		// BINARY[] (RFC 3516) — decode Content-Transfer-Encoding (base64,
		// quoted-printable) so the client gets the raw bytes. Without a
		// part spec we decode message-level CTE; multipart-walk (BINARY[1])
		// returns the section unchanged when MIME parsing is non-trivial.
		for _, section := range opts.BinarySection {
			if m.Filename == "" {
				break
			}
			rc, ferr := s.box.Fetch(s.folder.Name, m.Filename)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			decoded := decodeBinarySection(body, section.Part)
			s.statsFetchBody++
			s.statsFetchBodyB += int64(len(decoded))
			bw := mw.WriteBinarySection(section, int64(len(decoded)))
			io.Copy(bw, bytes.NewReader(decoded)) //nolint:errcheck
			bw.Close()
		}
		// BINARY.SIZE[] — same decode, return size only.
		for _, section := range opts.BinarySectionSize {
			if m.Filename == "" {
				break
			}
			rc, ferr := s.box.Fetch(s.folder.Name, m.Filename)
			if ferr != nil {
				break
			}
			body, _ := io.ReadAll(rc)
			rc.Close()
			decoded := decodeBinarySection(body, section.Part)
			mw.WriteBinarySectionSize(section, uint32(len(decoded)))
		}
		mw.Close() //nolint:errcheck
	}
	return nil
}

func (s *session) Store(w *imapserver.FetchWriter, numSet imaplib.NumSet, storeFlags *imaplib.StoreFlags, opts *imaplib.StoreOptions) error {
	if s.folder == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "No mailbox selected"}
	}
	msgs, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{})
	if err != nil {
		return err
	}

	// CONDSTORE STORE (UNCHANGEDSINCE N) — RFC 7162 §3.1.3.
	// Any message whose current modseq is greater than the client's
	// last-known value is *skipped* (no flag update, no FETCH response).
	// The list of skipped UIDs is returned as the MODIFIED response code
	// after STORE completes so the client can re-sync those messages.
	unchangedSince := uint64(0)
	if opts != nil {
		unchangedSince = opts.UnchangedSince
	}
	var modifiedUIDs imaplib.UIDSet

	for i, m := range msgs {
		seqNum := uint32(i + 1)
		if !numSetContains(numSet, seqNum, imaplib.UID(m.UID)) {
			continue
		}
		if unchangedSince > 0 && m.ModSeq > unchangedSince {
			modifiedUIDs.AddNum(imaplib.UID(m.UID))
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

		// Re-read modseq after UpdateFlags so the FETCH response carries
		// the bumped value (CONDSTORE clients use it to update their
		// last-known state).
		newModSeq := m.ModSeq
		if updated, err := s.idx.GetMessages(s.folder.ID, mailbox.SeqSet{{From: m.UID, To: m.UID}}); err == nil && len(updated) > 0 {
			newModSeq = updated[0].ModSeq
		}

		if !storeFlags.Silent {
			mw := w.CreateMessage(seqNum)
			mw.WriteFlags(toImapFlags(append(newFlags, newKW...)))
			mw.WriteUID(imaplib.UID(m.UID))
			if newModSeq > 0 {
				mw.WriteModSeq(newModSeq)
			}
			mw.Close() //nolint:errcheck
		}
	}

	if len(modifiedUIDs) > 0 {
		return &imaplib.Error{
			Type: imaplib.StatusResponseTypeOK,
			Code: imaplib.ResponseCode(string(imaplib.ResponseCodeModified) + " " + modifiedUIDs.String()),
			Text: "Some messages had a modseq greater than the supplied UNCHANGEDSINCE value",
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

// GetMetadata implements RFC 5464 GETMETADATA. mailbox == "" requests
// server-scope annotations (stored under INBOX's GUID with a vendor
// prefix so they cannot collide with INBOX's own mailbox attributes).
// Per RFC 5464, options.Depth controls whether entries below the
// requested name are included (0 = exact, 1 = direct children,
// infinity = whole subtree); options.MaxSize lets the server elide
// entries larger than the supplied byte cap.
func (s *session) GetMetadata(folder string, entries []string, opts *imaplib.GetMetadataOptions) (*imaplib.GetMetadataData, error) {
	if s.srv.opts.MetadataDict == nil {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata storage not configured"}
	}
	guid, err := s.metadataFolderGUID(folder)
	if err != nil {
		return nil, err
	}
	depth := imaplib.GetMetadataDepthZero
	var maxSize *uint32
	if opts != nil {
		depth = opts.Depth
		maxSize = opts.MaxSize
	}
	out := map[string]*[]byte{}
	for _, entry := range entries {
		scope, attrName, err := mailbox.ParseAttrEntry(entry)
		if err != nil {
			return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
		}
		if err := s.collectMetadata(folder, guid, scope, attrName, entry, depth, maxSize, out); err != nil {
			return nil, err
		}
	}
	return &imaplib.GetMetadataData{Mailbox: folder, Entries: out}, nil
}

// collectMetadata pulls either an exact key or a prefix iteration into out.
// Depth 0 = the entry itself; Depth 1 = entry + immediate children;
// Depth Infinity = entry + whole subtree.
func (s *session) collectMetadata(
	folder string, guid [16]byte,
	scope mailbox.AttrScope, attrName, requestedEntry string,
	depth imaplib.GetMetadataDepth, maxSize *uint32,
	out map[string]*[]byte,
) error {
	ctx := context.Background()
	ops := s.metadataOps()

	exactKey := s.metadataKey(folder, guid, scope, attrName)
	exactVals, found, err := s.srv.opts.MetadataDict.Lookup(ctx, ops, exactKey)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata lookup failed: " + err.Error()}
	}
	if found && len(exactVals) > 0 {
		v := exactVals[0]
		if maxSize == nil || uint32(len(v)) <= *maxSize {
			out[requestedEntry] = &v
		}
	}

	if depth == imaplib.GetMetadataDepthZero {
		return nil
	}

	flags := dict.IterSortByKey
	if depth == imaplib.GetMetadataDepthInfinity {
		flags |= dict.IterRecurse
	}
	prefix := s.metadataPrefix(folder, guid, scope) + attrName + "/"
	it, err := s.srv.opts.MetadataDict.Iterate(ctx, ops, prefix, flags)
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata iterate failed: " + err.Error()}
	}
	defer it.Close() //nolint:errcheck
	stripPrefix := s.metadataPrefix(folder, guid, scope)
	for it.Next() {
		k := it.Key()
		vs := it.Values()
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		if maxSize != nil && uint32(len(v)) > *maxSize {
			continue
		}
		entryName := mailbox.TrimAttrPrefix(k, stripPrefix)
		if entryName == "" {
			continue
		}
		out[mailbox.FormatAttrEntry(scope, entryName)] = &v
	}
	return it.Err()
}

// SetMetadata implements RFC 5464 SETMETADATA. A nil value in entries
// means "remove that attribute" (per the spec). Server-scope ops use
// mailbox == "".
func (s *session) SetMetadata(folder string, entries map[string]*[]byte) error {
	if s.srv.opts.MetadataDict == nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata storage not configured"}
	}
	guid, err := s.metadataFolderGUID(folder)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.srv.opts.MetadataDict.Begin(ctx, s.metadataOps())
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata begin failed: " + err.Error()}
	}
	for entry, value := range entries {
		scope, attrName, err := mailbox.ParseAttrEntry(entry)
		if err != nil {
			_ = tx.Rollback()
			return &imaplib.Error{Type: imaplib.StatusResponseTypeBad, Text: err.Error()}
		}
		key := s.metadataKey(folder, guid, scope, attrName)
		if value == nil {
			if err := tx.Unset(key); err != nil {
				_ = tx.Rollback()
				return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata unset failed: " + err.Error()}
			}
			continue
		}
		if err := tx.Set(key, *value); err != nil {
			_ = tx.Rollback()
			return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata set failed: " + err.Error()}
		}
	}
	res, err := tx.Commit()
	if err != nil {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata commit failed: " + err.Error()}
	}
	if res != dict.CommitOK {
		return &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Metadata commit returned " + strconv.Itoa(int(res))}
	}
	return nil
}

// metadataFolderGUID resolves the GUID used for keying the requested
// folder's attributes. Server-scope ops (folder == "") always hash under
// INBOX's GUID.
func (s *session) metadataFolderGUID(folder string) ([16]byte, error) {
	target := folder
	if target == "" {
		target = "INBOX"
	}
	f, err := s.ensureFolder(target)
	if err != nil {
		return [16]byte{}, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Mailbox lookup failed: " + err.Error()}
	}
	if f.GUID == ([16]byte{}) {
		return [16]byte{}, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Text: "Mailbox missing GUID"}
	}
	return f.GUID, nil
}

func (s *session) metadataKey(folder string, guid [16]byte, scope mailbox.AttrScope, attrName string) string {
	if folder == "" {
		return mailbox.ServerAttrKey(scope, guid, attrName)
	}
	return mailbox.AttrKey(scope, guid, attrName)
}

func (s *session) metadataPrefix(folder string, guid [16]byte, scope mailbox.AttrScope) string {
	if folder == "" {
		return mailbox.ServerAttrPrefix(scope, guid)
	}
	return mailbox.AttrPrefix(scope, guid)
}

func (s *session) metadataOps() *dict.OpSettings {
	if s.userInfo == nil {
		return nil
	}
	return &dict.OpSettings{
		Username: s.userInfo.Username,
		HomeDir:  s.userInfo.Home,
	}
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
	// CONDSTORE SEARCH MODSEQ filter — RFC 7162 §3.1.5.
	// Match only messages whose modseq is >= the supplied value.
	// MetadataName/MetadataType narrow which attribute's modseq to compare;
	// per-attribute modseq tracking is future work, so we treat any
	// criteria.ModSeq as "message-level modseq" and ignore the attribute
	// qualifier — strictly more permissive (returns extra matches), which
	// is RFC-acceptable.
	if criteria.ModSeq != nil && m.ModSeq < criteria.ModSeq.ModSeq {
		return false
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
