package snowflake

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	sf "github.com/snowflakedb/gosnowflake"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

func num(n int) error { return &sf.SnowflakeError{Number: n} }

func TestClassify(t *testing.T) {
	assert.ErrorIs(t, classify(num(codeInsufficientPrivs)), store.ErrForbidden)
	assert.ErrorIs(t, classify(num(codeNoSuchObject)), store.ErrForbidden)
	assert.ErrorIs(t, classify(num(codeSessionExpired)), store.ErrUnavailable)
	assert.ErrorIs(t, classify(num(codeLockTimeout)), store.ErrUnavailable)
	assert.ErrorIs(t, classify(num(codeTxLockContention)), store.ErrUnavailable)
	assert.ErrorIs(t, classify(num(390100)), store.ErrUnavailable)
	assert.ErrorIs(t, classify(context.DeadlineExceeded), store.ErrUnavailable)
	assert.ErrorIs(t, classify(driver.ErrBadConn), store.ErrUnavailable)
	require.NoError(t, classify(nil))

	plain := errors.New("x")
	assert.Equal(t, plain, classify(plain), "unknown errors pass through")
	var locked *store.ErrLocked
	require.ErrorAs(t, classify(&store.ErrLocked{}), &locked, "typed errors pass through")
}

func TestTransient(t *testing.T) {
	assert.True(t, transient(num(codeSessionExpired)))
	assert.True(t, transient(num(codeWarehouseSuspended)))
	assert.True(t, transient(driver.ErrBadConn))
	assert.False(t, transient(num(390100)), "login failures need a human")
	assert.False(t, transient(num(codePrimaryKeyViolation)))
	assert.False(t, transient(context.Canceled))
	assert.False(t, transient(nil))
}

func TestRetryStopsOnSuccessAndNonTransient(t *testing.T) {
	calls := 0
	require.NoError(t, retry(context.Background(), func() error {
		calls++
		if calls == 1 {
			return driver.ErrBadConn
		}
		return nil
	}))
	assert.Equal(t, 2, calls)

	calls = 0
	boom := errors.New("boom")
	require.ErrorIs(t, retry(context.Background(), func() error { calls++; return boom }), boom)
	assert.Equal(t, 1, calls)

	calls = 0
	err := retry(context.Background(), func() error { calls++; return driver.ErrBadConn })
	require.ErrorIs(t, err, store.ErrUnavailable)
	assert.Equal(t, maxAttempts, calls)
}

func TestProjectSchema(t *testing.T) {
	assert.Equal(t, "P_PLATFORM", ProjectSchema("platform"))
	assert.Equal(t, "P_MY_APP_2", ProjectSchema("my-app.2"))
}

func TestSchemaRouting(t *testing.T) {
	shared := New(Options{Database: "D", Schema: "BACKEND"})
	assert.Equal(t, "D.BACKEND.LOCKS", shared.dataTable("a/b", "LOCKS"))
	assert.Equal(t, "D.BACKEND.TOKENS", shared.controlTable("TOKENS"))

	split := New(Options{Database: "D", Schema: "BACKEND", Tenancy: TenancySchemaPerProject})
	assert.Equal(t, "D.P_A.LOCKS", split.dataTable("a/b", "LOCKS"))
	assert.Equal(t, "D.CONTROL.TOKENS", split.controlTable("TOKENS"))
	assert.Equal(t, "D.CONTROL.AUDIT_LOG", split.dataTable("", "AUDIT_LOG"))
}

func TestValidateRejectsUnsafeNames(t *testing.T) {
	require.NoError(t, New(Options{Database: "D", Schema: "S_1"}).Validate())
	require.Error(t, New(Options{Database: "D; DROP", Schema: "S"}).Validate())
	require.Error(t, New(Options{Database: "D", Schema: `S"`}).Validate())
}

func TestBlobPathGuard(t *testing.T) {
	require.NoError(t, checkBlobPath("platform/net/prod/0192.json.gz"))
	require.Error(t, checkBlobPath("a'; DROP"))
	require.Error(t, checkBlobPath("../x y"))
}

func TestMigrationsLoadAndRender(t *testing.T) {
	list, err := loadMigrations("migrations")
	require.NoError(t, err)
	require.NotEmpty(t, list)
	assert.Equal(t, 1, list[0].version)

	hybrid := New(Options{Database: "D", Schema: "S"}).render(list[0].sql)
	assert.Contains(t, hybrid, "CREATE HYBRID TABLE IF NOT EXISTS LOCKS")
	assert.NotContains(t, hybrid, "{{")
	assert.Contains(t, hybrid, "LOCK_ID      VARCHAR(128)  NOT NULL")

	standard := New(Options{Database: "D", Schema: "S", LockMode: LockStandard}).render(list[0].sql)
	assert.Contains(t, standard, "CREATE TABLE IF NOT EXISTS LOCKS")
	assert.NotContains(t, standard, "LOCK_ID      VARCHAR(128)  NOT NULL")

	stmts := statements(hybrid)
	assert.Len(t, stmts, 6)
	for _, s := range stmts {
		assert.False(t, strings.HasSuffix(s, ";"))
		assert.NotContains(t, s, "--")
	}
}
