package install

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thunter/snowstate/deploy"
)

func TestEmbeddedSeedHasOneKeyPlaceholder(t *testing.T) {
	require.Equal(t, 1, strings.Count(deploy.SeedSQL, "'<public key body>'"))
}
