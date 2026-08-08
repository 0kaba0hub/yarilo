package main

import (
	"strings"
	"testing"
)

// Every usage string and doc prints a plane word, and the pods set
// YARILO_ADMIN_TYPE, which makes that word implicit. Copying the line the
// tool just printed must therefore work -- it answered with an error about
// service names before (#1188). A word naming ANOTHER plane stays an error,
// and says which plane this container is.
func TestPlaneWordIsAcceptedWhenItAgreesWithTheEnvironment(t *testing.T) {
	cases := []struct {
		name      string
		adminType string
		args      []string
		wantErr   string // substring; "" means "must not complain about the plane"
	}{
		// The line the tool prints, run where it was printed.
		{"printed form inside a backend pod", "backend", []string{"backend", "index"}, ""},
		// The form that already worked.
		{"bare form inside a backend pod", "backend", []string{"index"}, ""},
		{"printed form inside a director pod", "director", []string{"director", "status"}, ""},
		// A word for another plane is a real mistake: name the plane, do not
		// send the reader to a list of services.
		{"other plane inside a backend pod", "backend", []string{"director", "status"}, "different one"},
		{"other plane inside a director pod", "director", []string{"backend", "index"}, "different one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("YARILO_ADMIN_TYPE", tc.adminType)
			err := dispatch(tc.args)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
				}
			default:
				// The command itself may fail for want of a server; what must
				// not happen is a complaint about the plane word.
				if err != nil && (strings.Contains(err.Error(), "unknown backend service") ||
					strings.Contains(err.Error(), "different one")) {
					t.Fatalf("the plane word was rejected: %v", err)
				}
			}
		})
	}
}
