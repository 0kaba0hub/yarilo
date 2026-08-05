package mailbox

import "io"

// Validating wraps a backend so every folder name is checked before a driver
// turns it into a path.
//
// It sits here, above the drivers, because a check inside one driver is a check
// one driver has. Two of the three had none, and the name arrives from six
// protocols: IMAP, LMTP (Sieve fileinto names the folder from a user-supplied
// script), POP3, JMAP, ManageSieve and the backend API. Putting the rule at the
// construction point makes it unbypassable by construction rather than by
// remembering -- the property #1063 turned out not to have (#1069).
//
// The drivers keep their own refusals. Not as a second copy of these rules, but
// as one: a path that resolved onto the mailbox root is not carried out. "The
// caller checked" is a promise, not a property, and it is the only thing left
// between a mistake and an account once validation moves up here.
func Validating(b MailboxBackend, rules NameRules) MailboxBackend {
	return &validatingBackend{inner: b, rules: rules}
}

type validatingBackend struct {
	inner MailboxBackend
	rules NameRules
}

func (v *validatingBackend) OpenUser(info *UserInfo) UserMailbox {
	return &validatingUser{
		inner:     v.inner.OpenUser(info),
		rules:     v.rules,
		nsSep:     SepOrDefault(info.Separator),
		layoutSep: LayoutSeparator(info.Driver),
	}
}

type validatingUser struct {
	inner     UserMailbox
	rules     NameRules
	nsSep     string
	layoutSep string
}

func (v *validatingUser) check(folder string) error {
	return ValidateName(folder, v.nsSep, v.layoutSep, v.rules)
}

func (v *validatingUser) Init() error { return v.inner.Init() }

func (v *validatingUser) Create(folder string) error {
	if err := v.check(folder); err != nil {
		return err
	}
	return v.inner.Create(folder)
}

func (v *validatingUser) Delete(folder string) error {
	if err := v.check(folder); err != nil {
		return err
	}
	return v.inner.Delete(folder)
}

func (v *validatingUser) Rename(oldName, newName string) error {
	if err := v.check(oldName); err != nil {
		return err
	}
	if err := v.check(newName); err != nil {
		return err
	}
	return v.inner.Rename(oldName, newName)
}

func (v *validatingUser) Save(folder string, r io.Reader, uid uint32, size int64, flags []string, guid [16]byte) (string, uint32, [16]byte, error) {
	if err := v.check(folder); err != nil {
		return "", 0, [16]byte{}, err
	}
	return v.inner.Save(folder, r, uid, size, flags, guid)
}

func (v *validatingUser) Move(srcFolder, dstFolder, filename string, guid [16]byte) (string, [16]byte, error) {
	if err := v.check(srcFolder); err != nil {
		return "", [16]byte{}, err
	}
	if err := v.check(dstFolder); err != nil {
		return "", [16]byte{}, err
	}
	return v.inner.Move(srcFolder, dstFolder, filename, guid)
}

func (v *validatingUser) Fetch(folder, filename string, altTier bool) (io.ReadCloser, error) {
	if err := v.check(folder); err != nil {
		return nil, err
	}
	return v.inner.Fetch(folder, filename, altTier)
}

func (v *validatingUser) Remove(folder, filename string) error {
	if err := v.check(folder); err != nil {
		return err
	}
	return v.inner.Remove(folder, filename)
}

func (v *validatingUser) List(folder string) ([]*MessageMeta, error) {
	if err := v.check(folder); err != nil {
		return nil, err
	}
	return v.inner.List(folder)
}

// FolderExists answers false for a name no folder may have, rather than
// refusing. Every caller asks it to decide between "answer for this mailbox"
// and "say it is not there", and an invalid name is not there.
func (v *validatingUser) FolderExists(folder string) (bool, error) {
	if err := v.check(folder); err != nil {
		return false, nil
	}
	return v.inner.FolderExists(folder)
}

func (v *validatingUser) ListFolders() ([]FolderEntry, error) { return v.inner.ListFolders() }

func (v *validatingUser) Scan(folder string) ([]ScanRecord, error) {
	if err := v.check(folder); err != nil {
		return nil, err
	}
	return v.inner.Scan(folder)
}

func (v *validatingUser) Close() error { return v.inner.Close() }

// Unwrap returns the driver handle underneath. Optional driver capabilities
// (mdbox purge, alt-move, proactive maildir sync, reactive heal) are reached by
// type assertion, and a wrapper hides every one of them -- so a call site that
// asks the wrapper whether it can purge is told no, however capable the driver
// is.
//
// Callers therefore assert against Unwrap, not against the handle. A site that
// forgets loses a capability rather than a check: the endpoint answers "this
// driver does not support it", which is visible and harmless, whereas the
// reverse arrangement would lose the check silently.
func (v *validatingUser) Unwrap() UserMailbox { return v.inner }

// Driver peels any wrappers off box so an optional-capability assertion sees
// the driver's own handle.
func Driver(box UserMailbox) UserMailbox {
	for {
		u, ok := box.(interface{ Unwrap() UserMailbox })
		if !ok {
			return box
		}
		box = u.Unwrap()
	}
}

// Unwrap returns the wrapped backend, the factory-level counterpart of the
// per-handle Unwrap.
func (v *validatingBackend) Unwrap() MailboxBackend { return v.inner }

// DriverBackend peels any wrappers off b. Used where a caller needs the
// driver's own type, such as a test asserting which driver a namespace built.
func DriverBackend(b MailboxBackend) MailboxBackend {
	for {
		u, ok := b.(interface{ Unwrap() MailboxBackend })
		if !ok {
			return b
		}
		b = u.Unwrap()
	}
}

// NameValidator is implemented by a wrapped backend that checks folder names.
type NameValidator interface {
	ValidateName(folder string) error
}

func (v *validatingUser) ValidateName(folder string) error { return v.check(folder) }

// CheckName applies the deployment's folder-name rules without performing an
// operation, for a command that has a name to judge but nothing to do with it
// yet -- SUBSCRIBE may name a mailbox that does not exist (RFC 9051 6.3.7), but
// storing a name no other command will ever accept can only produce a
// subscription that cannot be acted on.
//
// An unwrapped backend validates nothing and says so by returning nil, rather
// than this reaching for a default set of rules the deployment did not choose.
func CheckName(box UserMailbox, folder string) error {
	if nv, ok := box.(NameValidator); ok {
		return nv.ValidateName(folder)
	}
	return nil
}
