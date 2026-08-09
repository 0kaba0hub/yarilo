package buildmail

import (
	"io"

	"golang.org/x/net/html"
)

// htmlToText streams HTML text content into sink, dropping
// script/style/head subtrees and emitting a space at tag boundaries so
// adjacent elements don't fuse into one token.
// HTMLToText is exported for the JMAP snippet, which shows a fragment to a
// reader and must not put markup in front of them.
func HTMLToText(r io.Reader, sink func([]byte) error) error {
	tk := html.NewTokenizer(r)
	skipDepth := 0
	for {
		switch tk.Next() {
		case html.ErrorToken:
			if tk.Err() == io.EOF {
				return nil
			}
			// Malformed HTML: keep what was extracted.
			return nil
		case html.TextToken:
			if skipDepth == 0 {
				if err := sink(tk.Text()); err != nil {
					return err
				}
			}
		case html.StartTagToken:
			name, _ := tk.TagName()
			if skippedTag(string(name)) {
				skipDepth++
			}
			if err := sink([]byte(" ")); err != nil {
				return err
			}
		case html.EndTagToken:
			name, _ := tk.TagName()
			if skippedTag(string(name)) && skipDepth > 0 {
				skipDepth--
			}
			if err := sink([]byte(" ")); err != nil {
				return err
			}
		case html.SelfClosingTagToken:
			if err := sink([]byte(" ")); err != nil {
				return err
			}
		}
	}
}

func skippedTag(name string) bool {
	switch name {
	case "script", "style", "head":
		return true
	}
	return false
}
