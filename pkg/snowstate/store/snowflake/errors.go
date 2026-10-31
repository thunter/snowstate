package snowflake

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	sf "github.com/snowflakedb/gosnowflake/v2"

	"github.com/thunter/snowstate/pkg/snowstate/store"
)

// Snowflake error numbers this store acts on. Matching is on numbers, never on
// message text. See docs/decisions.md for how each was verified.
const (
	codePrimaryKeyViolation = 200001 // hybrid table duplicate key, SQLSTATE 22000
	codeNoSuchObject        = 2003   // "does not exist or not authorized"
	codeInsufficientPrivs   = 3001
	codeSchemaNotUsable     = 90105 // no current database or schema
	codeSessionExpired      = 390114
	codeWarehouseSuspended  = 606
	codeStatementTimeout    = 630
	codeLockTimeout         = 625
	codeLockWaitExceeded    = 628
	// codeTxLockContention: inside a transaction, a statement that would wait
	// on a hybrid row lock is aborted instead. Autocommit statements wait.
	codeTxLockContention = 200010
	// codeTooManyLockWaiters: too many transactions queued on one row.
	codeTooManyLockWaiters = 200012
	// codeFileNotFound: DownloadStream found nothing at the stage location.
	codeFileNotFound = 264006
	codeIPNotAllowed = 390432
)

func errorNumber(err error) int {
	var se *sf.SnowflakeError
	if errors.As(err, &se) {
		return se.Number
	}
	return 0
}

func isPrimaryKeyViolation(err error) bool { return errorNumber(err) == codePrimaryKeyViolation }

// IsNetworkPolicyRejection reports a login refused because of the client IP.
// The number is from Snowflake's documented behaviour and was not reproduced
// here, as the test account has no network policy, so the message is a fallback.
func IsNetworkPolicyRejection(err error) bool {
	return errorNumber(err) == codeIPNotAllowed ||
		(err != nil && strings.Contains(err.Error(), "is not allowed to access Snowflake"))
}

func isLockContention(err error) bool {
	n := errorNumber(err)
	return n == codeTxLockContention || n == codeTooManyLockWaiters
}

// transient reports whether retrying the statement could help.
func transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	switch n := errorNumber(err); {
	case n == codeSessionExpired, n == codeWarehouseSuspended:
		return true
	case n >= 390000 && n < 391000:
		return false // other session and login errors need a human
	}
	var netErr net.Error
	var urlErr *url.Error
	return errors.As(err, &netErr) || errors.As(err, &urlErr)
}

// classify maps a driver error to a typed store error, keeping the cause.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	switch errorNumber(err) {
	case codeInsufficientPrivs, codeNoSuchObject:
		return fmt.Errorf("%w: %w", store.ErrForbidden, err)
	case codeStatementTimeout, codeLockTimeout, codeLockWaitExceeded, codeTxLockContention, codeTooManyLockWaiters:
		return fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	if transient(err) || errorNumber(err) >= 390000 {
		return fmt.Errorf("%w: %w", store.ErrUnavailable, err)
	}
	return err
}
