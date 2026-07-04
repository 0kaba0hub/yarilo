package maildir

import "github.com/0kaba0hub/yarilo/internal/storage/mailbox/mboxenc"

func toModUTF7(s string) string            { return mboxenc.ToModUTF7(s) }
func fromModUTF7(s string) (string, error) { return mboxenc.FromModUTF7(s) }
func nfcNormalize(s string) string         { return mboxenc.NFC(s) }
