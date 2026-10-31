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
