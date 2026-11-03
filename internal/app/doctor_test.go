package app

import (
	"bytes"
	"errors"
	"testing"

	sf "github.com/snowflakedb/gosnowflake/v2"
	"github.com/stretchr/testify/assert"
)

func TestNetworkPolicyHint(t *testing.T) {
	var out bytes.Buffer
	printConnectivityHint(errors.New("something else"), &out)
	assert.Empty(t, out.String())

	printConnectivityHint(&sf.SnowflakeError{Number: 390432}, &out)
	for _, want := range []string{"network policy", "published Actions IP ranges", "static IPs", "self-hosted"} {
		assert.Contains(t, out.String(), want)
	}

	out.Reset()
	printConnectivityHint(errors.New("IP 1.2.3.4 is not allowed to access Snowflake"), &out)
	assert.NotEmpty(t, out.String())
}

func TestSecondaryRolesWarning(t *testing.T) {
	var out bytes.Buffer
	// What CURRENT_SECONDARY_ROLES() returns with them off, and on.
	printSecondaryRolesWarning(`{"roles":"","value":""}`, "TF_STATE_SVC", &out)
	assert.Empty(t, out.String())

	printSecondaryRolesWarning(`{"roles":"SNOWSTATE_SEED,TF_STATE_ADMIN","value":"ALL"}`, "TF_STATE_SVC", &out)
	for _, want := range []string{"WARN", "SNOWSTATE_SEED,TF_STATE_ADMIN", "can't prove the grants of TF_STATE_SVC", "DEFAULT_SECONDARY_ROLES = ()"} {
		assert.Contains(t, out.String(), want)
	}
}

func TestSecondaryRolesWarningWhenUnreadable(t *testing.T) {
	var out bytes.Buffer
	printSecondaryRolesWarning("not json", "TF_STATE_SVC", &out)
	assert.Contains(t, out.String(), "WARN")
	assert.Contains(t, out.String(), "couldn't read")
}
