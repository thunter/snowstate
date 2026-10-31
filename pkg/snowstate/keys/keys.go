// Package keys implements the state key and workspace convention (SPEC §4.2).
//
// Every state key is <prefix>/<workspace>. The prefix has 1-15 segments, of
// which the first is the project. The workspace is exactly one segment.
// Frontends and the CLI must all use this package so that an HTTP address and
// a provider prefix+workspace pair resolve to the same row.
package keys

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	// DefaultWorkspace is Terraform's default workspace name.
	DefaultWorkspace = "default"
	// MaxKeyLength is the longest allowed key.
	MaxKeyLength = 512
	// MaxSegments is the largest number of segments in a key.
	MaxSegments = 16
	// lockSegment is reserved for the /lock route.
	lockSegment = "lock"
)

// ErrInvalid is wrapped by every validation error from this package.
var ErrInvalid = errors.New("invalid state key")

var (
	segmentRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	keyRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*){0,15}$`)
)

// Validate checks a full state key.
func Validate(key string) error {
	if len(key) == 0 || len(key) > MaxKeyLength {
		return fmt.Errorf("%w: length must be 1-%d", ErrInvalid, MaxKeyLength)
	}
	if !keyRE.MatchString(key) {
		return fmt.Errorf("%w: %q does not match the segment rules", ErrInvalid, key)
	}
	if !strings.Contains(key, "/") {
		return fmt.Errorf("%w: %q needs at least <project>/<workspace>", ErrInvalid, key)
	}
	if key[strings.LastIndexByte(key, '/')+1:] == lockSegment {
		return fmt.Errorf("%w: the final segment cannot be %q", ErrInvalid, lockSegment)
	}
	return nil
}

// ValidatePrefix checks a prefix, which is a key without its workspace.
func ValidatePrefix(prefix string) error {
	if len(prefix) == 0 || len(prefix) > MaxKeyLength {
		return fmt.Errorf("%w: prefix length must be 1-%d", ErrInvalid, MaxKeyLength)
	}
	if !keyRE.MatchString(prefix) {
		return fmt.Errorf("%w: prefix %q does not match the segment rules", ErrInvalid, prefix)
	}
	if strings.Count(prefix, "/")+1 >= MaxSegments {
		return fmt.Errorf("%w: prefix %q has too many segments", ErrInvalid, prefix)
	}
	return nil
}

// ValidateWorkspace checks a workspace name. These rules are stricter than
// Terraform's own, so the error says so.
func ValidateWorkspace(name string) error {
	if !segmentRE.MatchString(name) {
		return fmt.Errorf("%w: workspace %q must match %s", ErrInvalid, name, segmentRE)
	}
	if name == lockSegment {
		return fmt.Errorf("%w: workspace cannot be named %q", ErrInvalid, lockSegment)
	}
	return nil
}

// Split separates a key into its prefix and workspace.
func Split(key string) (prefix, workspace string, err error) {
	if err := Validate(key); err != nil {
		return "", "", err
	}
	i := strings.LastIndexByte(key, '/')
	return key[:i], key[i+1:], nil
}

// Join builds a key from a prefix and workspace.
func Join(prefix, workspace string) (string, error) {
	if err := ValidatePrefix(prefix); err != nil {
		return "", err
	}
	if err := ValidateWorkspace(workspace); err != nil {
		return "", err
	}
	key := prefix + "/" + workspace
	if err := Validate(key); err != nil {
		return "", err
	}
	return key, nil
}

// Project returns the first segment of a key or prefix.
func Project(key string) string {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i]
	}
	return key
}

// WorkspaceOf returns the workspace of key when key is exactly
// <prefix>/<one segment>. Deeper keys are not workspaces of that prefix.
func WorkspaceOf(prefix, key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, prefix+"/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}
