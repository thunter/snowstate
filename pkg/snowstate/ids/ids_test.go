package ids

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormats(t *testing.T) {
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), V4())
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), V7())
}

func TestV7IsTimeOrdered(t *testing.T) {
	a := V7()
	time.Sleep(2 * time.Millisecond)
	b := V7()
	assert.Less(t, a, b)
}
