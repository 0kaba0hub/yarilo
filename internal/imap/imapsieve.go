package imap

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/yarilomail/yarilo/internal/sieve"
	"github.com/yarilomail/yarilo/pkg/locks"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

const imapSieveScriptAttr = "imapsieve/script"

// imapSieveScriptName resolves the Sieve script bound to a mailbox for imapsieve
// (RFC 6785): the /shared/imapsieve/script METADATA annotation on the mailbox,
// falling back to the server-wide annotation under INBOX. "" = none bound.
func (s *session) imapSieveScriptName(h *nsHandle, rel string, guid [16]byte) string {
	if s.srv.opts.MetadataDict == nil {
		return ""
	}
	ctx := context.Background()
	ops := s.metadataOps()
	key := s.metadataKey(h, rel, guid, mailbox.AttrShared, imapSieveScriptAttr)
	if vals, found, err := s.srv.opts.MetadataDict.Lookup(ctx, ops, key); err == nil && found && len(vals) > 0 && len(vals[0]) > 0 {
		return string(vals[0])
	}
	if inbox, err := s.primary.idx.OpenFolder("INBOX", uint32(time.Now().Unix())); err == nil {
		skey := mailbox.ServerAttrKey(mailbox.AttrShared, inbox.GUID, imapSieveScriptAttr)
		if vals, found, err := s.srv.opts.MetadataDict.Lookup(ctx, ops, skey); err == nil && found && len(vals) > 0 && len(vals[0]) > 0 {
			return string(vals[0])
		}
	}
	return ""
}

// runImapSieveEvent runs imapsieve for one just-stored message and applies the
// resulting actions. h/rel/folderID/guid identify the mailbox the event occurred
// on; uid/filename/altTier the stored message. srcMailbox is the COPY/MOVE source
// (empty for APPEND).
func (s *session) runImapSieveEvent(cause, mailboxName, rel string, h *nsHandle, folderID uint64, guid [16]byte, uid uint32, filename string, altTier bool, srcMailbox string, changedFlags []string) {
	eng := s.srv.opts.SieveEngine
	if eng == nil {
		return
	}
	scriptName := s.imapSieveScriptName(h, rel, guid)

	rc, err := h.box.Fetch(rel, filename, altTier)
	if err != nil {
		s.flagCorruptOnRead(h.idx, folderID, rel, filename, uid, err)
		slog.Warn("imapsieve: fetch stored message", "user", s.userInfo.Username, "folder", rel, "err", err)
		return
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()

	res, err := eng.RunIMAPEvent(context.Background(), sieve.IMAPEventOptions{
		Username:     s.userInfo.Username,
		HomeDir:      s.userInfo.Home,
		Cause:        cause,
		Mailbox:      mailboxName,
		SrcMailbox:   srcMailbox,
		ChangedFlags: changedFlags,
		MsgRaw:       raw,
		ScriptName:   scriptName,
	})
	if err != nil {
		slog.Error("imapsieve: run", "user", s.userInfo.Username, "cause", cause, "folder", rel, "err", err)
		return
	}
	if res == nil {
		return
	}
	s.applyImapSieveResult(res, h, rel, folderID, uid, filename, raw)
}

// applyImapSieveResult applies imapsieve actions to the stored message. The
// message currently lives in (h, rel) as uid. Per RFC 6785: an implicit/explicit
// keep leaves it in place, fileinto copies it into the named mailbox (and, when
// keep is cancelled, the original is expunged — a move), discard expunges it,
// and imap4flags updates its flags.
func (s *session) applyImapSieveResult(res *sieve.FilterResult, h *nsHandle, rel string, folderID uint64, uid uint32, filename string, raw []byte) {
	// discard: the script cancelled keep and filed nowhere.
	if len(res.Deliveries) == 0 {
		s.imapSieveExpunge(h, rel, folderID, uid, filename)
		return
	}
	keepInPlace := false
	var keepFlags []string
	for _, d := range res.Deliveries {
		if d.FromKeep {
			keepInPlace = true
			keepFlags = d.Flags
			continue
		}
		if d.Folder == rel {
			// fileinto back into the same mailbox — already there.
			keepInPlace = true
			keepFlags = d.Flags
			continue
		}
		s.imapSieveFileInto(d.Folder, raw, d.Flags, d.Create)
	}
	if !keepInPlace {
		s.imapSieveExpunge(h, rel, folderID, uid, filename)
		return
	}
	if len(keepFlags) > 0 {
		if err := h.idx.UpdateFlags(folderID, uid, keepFlags, nil); err != nil {
			slog.Warn("imapsieve: update flags", "folder", rel, "uid", uid, "err", err)
		}
	}
}

// imapSieveFileInto stores a copy of raw into the named mailbox.
func (s *session) imapSieveFileInto(name string, raw []byte, flags []string, create bool) {
	dh, drel, df, err := s.ensureFolderHandle(name)
	if err != nil {
		slog.Warn("imapsieve: fileinto target", "folder", name, "err", err)
		return
	}
	if create {
		_ = dh.box.Create(drel) // idempotent for imapsieve fileinto :create
	}
	newFilename, vsize, guid, err := dh.box.Save(drel, bytes.NewReader(raw), 0, int64(len(raw)), flags, [16]byte{})
	if err != nil {
		slog.Warn("imapsieve: fileinto save", "folder", name, "err", err)
		return
	}
	nm := &mailbox.MessageMeta{
		Filename: newFilename, Flags: flags, Size: uint32(len(raw)), VSize: vsize, InternalDate: time.Now(), GUID: guid,
	}
	if err := dh.idx.AllocateAndAppend(df.ID, nm); err != nil {
		_ = dh.box.Remove(drel, newFilename)
		slog.Warn("imapsieve: fileinto record", "folder", name, "err", err)
		return
	}
	// Explicit write confirmation (#625): a sieve fileinto lands a message the same
	// way LMTP/APPEND do, but previously only logged on error — so a message that
	// "vanished" could not be told apart from one that was never written. Mirrors
	// the "imap: append saved" line; no message content, only the write coordinates.
	slog.Debug("imapsieve: fileinto saved",
		"user", s.userInfo.Username,
		"folder", name,
		"uid", nm.UID,
		"file", newFilename,
		"size", nm.Size,
	)
	s.emitMailboxChange(name, locks.EventDelivered, nm.UID)
}

// imapSieveExpunge removes the message from its current mailbox.
func (s *session) imapSieveExpunge(h *nsHandle, rel string, folderID uint64, uid uint32, filename string) {
	_ = h.box.Remove(rel, filename)
	if err := h.idx.ExpungeMessage(folderID, uid); err != nil {
		slog.Warn("imapsieve: expunge", "folder", rel, "uid", uid, "err", err)
		return
	}
	s.emitMailboxChange(rel, locks.EventExpunged, uid)
}
