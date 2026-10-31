package snowstate_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestImportBoundaries enforces the frontend-neutrality rules in SPEC §3.
// Packages that only the Snowflake store needs are checked separately, because
// gosnowflake itself imports net/http.
func TestImportBoundaries(t *testing.T) {
	forbiddenEverywhere := []string{"github.com/thunter/snowstate/internal/", "github.com/hashicorp/terraform"}

	all := run(t, "go", "list", "-deps", "../../pkg/...")
	for _, dep := range all {
		for _, bad := range forbiddenEverywhere {
			assert.False(t, strings.HasPrefix(dep, bad), "pkg/ depends on %s", dep)
		}
	}

	direct := run(t, "go", "list", "-f", `{{.ImportPath}} {{join .Imports " "}}`, "../../pkg/...")
	for _, line := range direct {
		fields := strings.Fields(line)
		for _, imp := range fields[1:] {
			if !strings.HasSuffix(fields[0], "/sfconn") {
				assert.NotEqual(t, "net/http", imp, "%s imports net/http", fields[0])
			}
			if imp == "github.com/snowflakedb/gosnowflake/v2" {
				assert.Contains(t, []string{
					"github.com/thunter/snowstate/pkg/snowstate/store/snowflake",
					"github.com/thunter/snowstate/pkg/snowstate/sfconn",
				}, fields[0], "only the Snowflake packages may import gosnowflake")
			}
		}
	}

	core := run(t, "go", "list", "-deps",
		"../../pkg/snowstate/service", "../../pkg/snowstate/keys", "../../pkg/snowstate/store/memory")
	assert.NotContains(t, core, "net/http", "the core must stay free of net/http, even transitively")
	assert.NotContains(t, core, "github.com/snowflakedb/gosnowflake/v2")
}

func run(t *testing.T, name string, args ...string) []string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}
