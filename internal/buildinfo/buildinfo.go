// Package buildinfo holds version metadata stamped in at link time.
package buildinfo

import "fmt"

// These are overridden with -ldflags by the Makefile and the release build.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns the one-line form printed by `snowstate version`.
func String() string {
	return fmt.Sprintf("snowstate %s (commit %s, built %s)", Version, Commit, Date)
}
