package sieve

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const debugLogName = ".yarilo.sieve.log"

// makeDebugLogger returns a callback for the vnd.yarilo.debug debug_log command.
// Messages are appended to <homeDir>/.yarilo.sieve.log with a timestamp prefix.
// If homeDir is empty the callback is a no-op.
func makeDebugLogger(homeDir string) func(msg string) {
	if homeDir == "" {
		return func(string) {}
	}
	path := filepath.Join(homeDir, debugLogName)
	return func(msg string) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s  %s\n", time.Now().UTC().Format(time.RFC3339), msg)
	}
}
