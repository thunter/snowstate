// Package deploy embeds the files snowstate ships for setting up an Install,
// so a binary-only user has them.
package deploy

import (
	"embed"
	"io/fs"
)

// SeedSQL is the Seed, with a placeholder for the public key body.
//
//go:embed snowflake/seed.sql
var SeedSQL string

//go:embed terraform/foundation
var foundation embed.FS

// Foundation returns the Foundation's Terraform root module, rooted at its
// own directory.
func Foundation() fs.FS {
	sub, err := fs.Sub(foundation, "terraform/foundation")
	if err != nil {
		panic(err) // the embed pattern above guarantees the directory
	}
	return sub
}
