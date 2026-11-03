package app

import (
	"errors"
	"fmt"
	"testing"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/stretchr/testify/assert"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

func TestMigrateHintPointsToTheAdminRole(t *testing.T) {
	denied := fmt.Errorf("%w: %w", store.ErrForbidden, &sf.SnowflakeError{Number: 3001, Message: "Insufficient privileges"})
	err := MigrateHint(denied, "TF_STATE_SVC")

	assert.ErrorIs(t, err, store.ErrForbidden)
	for _, want := range []string{"TF_STATE_SVC lacks a privilege migrate needs", "the Install's admin role", "<database>_ADMIN",
		"If it already is", "CREATE HYBRID TABLE"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestMigrateHintIsOnlyForMissingPrivileges(t *testing.T) {
	missing := fmt.Errorf("%w: %w", store.ErrForbidden, &sf.SnowflakeError{Number: 2003, Message: "does not exist or not authorized"})
	assert.Equal(t, missing, MigrateHint(missing, "TF_STATE_SVC"))

	other := errors.New("network down")
	assert.Equal(t, other, MigrateHint(other, "TF_STATE_SVC"))
	assert.NoError(t, MigrateHint(nil, "TF_STATE_SVC"))
}
