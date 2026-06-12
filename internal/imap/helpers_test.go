package imap

import (
	"bufio"
	"strings"
	"testing"
)

func readLineIMAP(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	return strings.TrimRight(line, "\r\n")
}
