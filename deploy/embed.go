// Package deploy embeds the files snowstate ships for setting up an Install,
// so a binary-only user has them.
package deploy

import _ "embed"

// SeedSQL is the Seed, with a placeholder for the public key body.
//
//go:embed snowflake/seed.sql
var SeedSQL string
