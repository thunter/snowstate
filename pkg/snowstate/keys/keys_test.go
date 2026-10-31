package keys

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	long := strings.Repeat("a", 300) + "/" + strings.Repeat("b", 300)
	seventeen := strings.TrimSuffix(strings.Repeat("a/", 17), "/")
	sixteen := strings.TrimSuffix(strings.Repeat("a/", 16), "/")
	valid := []string{"platform/default", "platform/network/prod", "a.b/c_d-e/F9", sixteen}
	invalid := []string{
		"", "platform", "/a/b", "a/b/", "a//b", "a/../b", "a/./b", "a/.hidden", "a/-x", "a/_x",
		"a b/c", "a/b%2fc", "a/lock", "a/b/lock", long, seventeen,
	}
	for _, k := range valid {
		assert.NoError(t, Validate(k), k)
	}
	for _, k := range invalid {
		assert.ErrorIs(t, Validate(k), ErrInvalid, k)
	}
}

func TestKeysAreCaseSensitiveLockOnlyInFinalSegment(t *testing.T) {
	assert.NoError(t, Validate("a/LOCK"))
	assert.NoError(t, Validate("lock/b"))
	assert.NoError(t, Validate("a/lock/b"))
}

func TestSplitAndJoin(t *testing.T) {
	p, w, err := Split("platform/network/prod")
	require.NoError(t, err)
	assert.Equal(t, "platform/network", p)
	assert.Equal(t, "prod", w)

	k, err := Join("platform/network", DefaultWorkspace)
	require.NoError(t, err)
	assert.Equal(t, "platform/network/default", k)

	_, _, err = Split("platform")
	require.Error(t, err)
	_, err = Join("platform", "has space")
	require.ErrorContains(t, err, "workspace")
	_, err = Join("platform", "lock")
	require.Error(t, err)
	_, err = Join("/bad", "w")
	require.Error(t, err)
	_, err = Join(strings.Repeat("a", 512), "w")
	require.Error(t, err)
}

func TestValidatePrefix(t *testing.T) {
	require.NoError(t, ValidatePrefix("platform"))
	require.NoError(t, ValidatePrefix(strings.TrimSuffix(strings.Repeat("a/", 15), "/")))
	require.Error(t, ValidatePrefix(strings.TrimSuffix(strings.Repeat("a/", 16), "/")))
	require.Error(t, ValidatePrefix(""))
	require.Error(t, ValidatePrefix("a//b"))
}

func TestProject(t *testing.T) {
	assert.Equal(t, "platform", Project("platform/network/prod"))
	assert.Equal(t, "platform", Project("platform"))
}

func TestWorkspaceOfListsOnlyDirectChildren(t *testing.T) {
	w, ok := WorkspaceOf("platform/net", "platform/net/prod")
	assert.True(t, ok)
	assert.Equal(t, "prod", w)

	for _, k := range []string{"platform/net/eu/prod", "platform/netx/prod", "platform/net", "other/net/prod"} {
		_, ok := WorkspaceOf("platform/net", k)
		assert.False(t, ok, k)
	}
}
