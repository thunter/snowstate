package app

import (
	"fmt"

	"github.com/thunter/snowstate/pkg/snowstate/store/snowflake"
)

// MigrateHint adds the fix to a migrate that Snowflake refused for lack of a
// privilege. Names vary between Installs, so it names the pattern rather than
// a role.
func MigrateHint(err error, role string) error {
	if !snowflake.IsInsufficientPrivileges(err) {
		return err
	}
	return fmt.Errorf("%w\n%s lacks a privilege migrate needs: run migrate as the Install's admin role, <database>_ADMIN "+
		"for a default Install. If it already is, check its grants on the schema: hybrid tables need CREATE HYBRID TABLE", err, role)
}
