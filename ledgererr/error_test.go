package ledgererr_test

import (
	"errors"
	"testing"

	"github.com/21strive/ledger/ledgererr"
	"github.com/stretchr/testify/assert"
)

func TestNewError(t *testing.T) {
	t.Run("create error without origin", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeInternal, "internal error", nil)

		assert.Equal(t, ledgererr.CodeInternal, err.Code)
		assert.Equal(t, "internal error", err.Msg)
		assert.Nil(t, err.OriginError)
	})

	t.Run("create error with origin", func(t *testing.T) {
		originErr := errors.New("database connection failed")
		err := ledgererr.NewError(ledgererr.CodeDatabaseError, "database error", originErr)

		assert.Equal(t, ledgererr.CodeDatabaseError, err.Code)
		assert.Equal(t, "database error", err.Msg)
		assert.Equal(t, originErr, err.OriginError)
	})
}

func TestAppError_Error(t *testing.T) {
	t.Run("error message without origin", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeNotFound, "resource not found", nil)

		assert.Equal(t, "resource not found", err.Error())
	})

	t.Run("error message with origin", func(t *testing.T) {
		originErr := errors.New("connection timeout")
		err := ledgererr.NewError(ledgererr.CodeGatewayAPIError, "gateway API failed", originErr)

		assert.Equal(t, "gateway API failed: connection timeout", err.Error())
	})

	t.Run("error message with nested AppError", func(t *testing.T) {
		innerErr := ledgererr.NewError(ledgererr.CodeDatabaseError, "query failed", errors.New("syntax error"))
		outerErr := ledgererr.NewError(ledgererr.CodeInternal, "operation failed", innerErr)

		assert.Contains(t, outerErr.Error(), "operation failed")
		assert.Contains(t, outerErr.Error(), "query failed")
	})
}

func TestAppError_WithError(t *testing.T) {
	t.Run("add origin error to existing error", func(t *testing.T) {
		baseErr := ledgererr.NewError(ledgererr.CodeNotFound, "not found", nil)
		originErr := errors.New("record does not exist")

		newErr := baseErr.WithError(originErr)

		assert.Equal(t, baseErr.Code, newErr.Code)
		assert.Equal(t, baseErr.Msg, newErr.Msg)
		assert.Equal(t, originErr, newErr.OriginError)
		assert.Nil(t, baseErr.OriginError) // Original should be unchanged
	})

	t.Run("replace origin error", func(t *testing.T) {
		firstOrigin := errors.New("first error")
		baseErr := ledgererr.NewError(ledgererr.CodeInternal, "internal", firstOrigin)
		secondOrigin := errors.New("second error")

		newErr := baseErr.WithError(secondOrigin)

		assert.Equal(t, secondOrigin, newErr.OriginError)
		assert.Equal(t, firstOrigin, baseErr.OriginError) // Original unchanged
	})
}

func TestAppError_ErrCode(t *testing.T) {
	t.Run("get code from simple error", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)

		assert.Equal(t, ledgererr.CodeDatabaseError, err.ErrCode())
	})

	t.Run("get code from nested AppError", func(t *testing.T) {
		innerErr := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)
		outerErr := ledgererr.NewError(ledgererr.CodeInternal, "internal error", innerErr)

		// Should return the inner error's code
		assert.Equal(t, ledgererr.CodeDatabaseError, outerErr.ErrCode())
	})

	t.Run("get code with non-AppError origin", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeGatewayAPIError, "gateway error", errors.New("standard error"))

		assert.Equal(t, ledgererr.CodeGatewayAPIError, err.ErrCode())
	})

	t.Run("get code from deeply nested AppErrors", func(t *testing.T) {
		level1 := ledgererr.NewError(ledgererr.CodeNotFound, "not found", nil)
		level2 := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", level1)
		level3 := ledgererr.NewError(ledgererr.CodeInternal, "internal", level2)

		// Should unwrap to the deepest AppError
		assert.Equal(t, ledgererr.CodeNotFound, level3.ErrCode())
	})
}

func TestAppError_Unwrap(t *testing.T) {
	t.Run("unwrap returns origin error", func(t *testing.T) {
		originErr := errors.New("original error")
		err := ledgererr.NewError(ledgererr.CodeInternal, "wrapped", originErr)

		assert.Equal(t, originErr, err.Unwrap())
	})

	t.Run("unwrap returns nil when no origin", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeInternal, "no origin", nil)

		assert.Nil(t, err.Unwrap())
	})

	t.Run("unwrap works with errors.Is", func(t *testing.T) {
		originErr := errors.New("sentinel error")
		err := ledgererr.NewError(ledgererr.CodeInternal, "wrapped", originErr)

		assert.True(t, errors.Is(err, originErr))
	})
}

func TestIsAppError(t *testing.T) {
	t.Run("detect same error by error code", func(t *testing.T) {
		err1 := ledgererr.NewError(ledgererr.CodeInternal, "first error", nil)
		err2 := ledgererr.NewError(ledgererr.CodeInternal, "second error", nil)

		assert.True(t, ledgererr.IsAppError(err1, err2))
	})

	t.Run("return false for different error codes", func(t *testing.T) {
		err1 := ledgererr.NewError(ledgererr.CodeInternal, "internal error", nil)
		err2 := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)

		assert.False(t, ledgererr.IsAppError(err1, err2))
	})

	t.Run("return false for non-AppError", func(t *testing.T) {
		standardErr := errors.New("standard error")
		appErr := ledgererr.NewError(ledgererr.CodeInternal, "app error", nil)

		assert.False(t, ledgererr.IsAppError(standardErr, appErr))
	})

	t.Run("detect nested AppError by error code", func(t *testing.T) {
		innerErr := ledgererr.NewError(ledgererr.CodeDatabaseError, "inner", nil)
		outerErr := ledgererr.NewError(ledgererr.CodeInternal, "outer", innerErr)
		compareErr := ledgererr.NewError(ledgererr.CodeDatabaseError, "compare", nil)

		// outerErr wraps innerErr which has CodeDatabaseError
		// So ErrCode() returns CodeDatabaseError
		assert.True(t, ledgererr.IsAppError(outerErr, compareErr))
	})

	t.Run("compare ErrLedgerAlreadyExists with itself", func(t *testing.T) {
		err := ledgererr.ErrLedgerAlreadyExists

		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists))
	})

	t.Run("compare ErrLedgerAlreadyExists with origin", func(t *testing.T) {
		dbErr := errors.New("unique constraint violation")
		err := ledgererr.ErrLedgerAlreadyExists.WithError(dbErr)

		// err has the same error code as ErrLedgerAlreadyExists
		assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists))
	})

	t.Run("compare different errors with same code", func(t *testing.T) {
		err1 := ledgererr.NewError(ledgererr.ErrorCode(409100), "ledger exists 1", nil)
		err2 := ledgererr.NewError(ledgererr.ErrorCode(409100), "ledger exists 2", nil)

		assert.True(t, ledgererr.IsAppError(err1, err2))
	})

	t.Run("return false when comparing different codes", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeNotFound, "not found", nil)

		assert.False(t, ledgererr.IsAppError(err, ledgererr.ErrLedgerAlreadyExists))
	})

	t.Run("deeply nested error uses innermost code", func(t *testing.T) {
		level1 := ledgererr.NewError(ledgererr.CodeNotFound, "not found", nil)
		level2 := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", level1)
		level3 := ledgererr.NewError(ledgererr.CodeInternal, "internal", level2)
		compareErr := ledgererr.NewError(ledgererr.CodeNotFound, "compare", nil)

		// level3 wraps down to level1 which has CodeNotFound
		assert.True(t, ledgererr.IsAppError(level3, compareErr))
	})
}

func TestErrorsIsAndAs(t *testing.T) {
	t.Run("identify ErrLedgerAlreadyExists using errors.Is", func(t *testing.T) {
		err := ledgererr.ErrLedgerAlreadyExists

		assert.True(t, errors.Is(err, ledgererr.ErrLedgerAlreadyExists))
	})

	t.Run("identify ErrLedgerAlreadyExists with origin using errors.As", func(t *testing.T) {
		dbErr := errors.New("unique constraint violation")
		err := ledgererr.ErrLedgerAlreadyExists.WithError(dbErr)

		var appErr ledgererr.AppError
		assert.True(t, errors.As(err, &appErr))
		assert.Equal(t, ledgererr.CodeLedgerAlreadyExists, appErr.Code)
		assert.Equal(t, "ledger already exists", appErr.Msg)
	})
}

func TestIsErrorCode(t *testing.T) {
	t.Run("detect matching error code", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)

		assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeDatabaseError, err))
	})

	t.Run("return false for different code", func(t *testing.T) {
		err := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)

		assert.False(t, ledgererr.IsErrorCode(ledgererr.CodeNotFound, err))
	})

	t.Run("return false for non-AppError", func(t *testing.T) {
		err := errors.New("standard error")

		assert.False(t, ledgererr.IsErrorCode(ledgererr.CodeInternal, err))
	})

	t.Run("detect code from nested AppError", func(t *testing.T) {
		innerErr := ledgererr.NewError(ledgererr.CodeNotFound, "not found", nil)
		outerErr := ledgererr.NewError(ledgererr.CodeInternal, "internal", innerErr)

		// Should detect the innermost error code
		assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeNotFound, outerErr))
		assert.False(t, ledgererr.IsErrorCode(ledgererr.CodeInternal, outerErr))
	})

	t.Run("detect code with multiple nesting levels", func(t *testing.T) {
		level1 := ledgererr.NewError(ledgererr.CodeGatewayAPIError, "gateway", nil)
		level2 := ledgererr.NewError(ledgererr.CodeDatabaseError, "db", level1)
		level3 := ledgererr.NewError(ledgererr.CodeInternal, "internal", level2)

		assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeGatewayAPIError, level3))
	})

	t.Run("identify ErrLedgerAlreadyExists by error code", func(t *testing.T) {
		err := ledgererr.ErrLedgerAlreadyExists

		assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeLedgerAlreadyExists, err))
	})

	t.Run("identify ErrLedgerAlreadyExists with origin by error code", func(t *testing.T) {
		dbErr := errors.New("duplicate key")
		err := ledgererr.ErrLedgerAlreadyExists.WithError(dbErr)

		// Should still identify by error code even with origin
		assert.True(t, ledgererr.IsErrorCode(ledgererr.CodeLedgerAlreadyExists, err))
	})
}

func TestErrorCodes(t *testing.T) {
	t.Run("verify error code values", func(t *testing.T) {
		assert.Equal(t, ledgererr.ErrorCode(500), ledgererr.CodeInternal)
		assert.Equal(t, ledgererr.ErrorCode(404), ledgererr.CodeNotFound)
		assert.Equal(t, ledgererr.ErrorCode(500001), ledgererr.CodeDatabaseError)
		assert.Equal(t, ledgererr.ErrorCode(500002), ledgererr.CodeGatewayAPIError)
		assert.Equal(t, ledgererr.ErrorCode(409001), ledgererr.CodeSubaccountAlreadyExists)
	})

	t.Run("error codes are unique", func(t *testing.T) {
		codes := []ledgererr.ErrorCode{
			ledgererr.CodeInternal,
			ledgererr.CodeNotFound,
			ledgererr.CodeDatabaseError,
			ledgererr.CodeGatewayAPIError,
			ledgererr.CodeSubaccountAlreadyExists,
		}

		seen := make(map[ledgererr.ErrorCode]bool)
		for _, code := range codes {
			assert.False(t, seen[code], "error code %d is duplicated", code)
			seen[code] = true
		}
	})
}

func TestPredefinedErrors(t *testing.T) {
	t.Run("ErrLedgerAlreadyExists", func(t *testing.T) {
		err := ledgererr.ErrLedgerAlreadyExists

		assert.Equal(t, ledgererr.CodeLedgerAlreadyExists, err.Code)
		assert.Equal(t, "ledger already exists", err.Msg)
		assert.Nil(t, err.OriginError)
	})

	t.Run("ErrLedgerAlreadyExists with origin", func(t *testing.T) {
		originErr := errors.New("duplicate key violation")
		err := ledgererr.ErrLedgerAlreadyExists.WithError(originErr)

		assert.Equal(t, ledgererr.CodeLedgerAlreadyExists, err.Code)
		assert.Equal(t, "ledger already exists", err.Msg)
		assert.Equal(t, originErr, err.OriginError)
	})
}

func TestErrorChaining(t *testing.T) {
	t.Run("chain multiple errors", func(t *testing.T) {
		dbErr := errors.New("connection refused")
		appErr1 := ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to connect", dbErr)
		appErr2 := ledgererr.NewError(ledgererr.CodeInternal, "operation failed", appErr1)

		// Should be able to unwrap through the chain
		assert.True(t, errors.Is(appErr2, dbErr))
		assert.Equal(t, ledgererr.CodeDatabaseError, appErr2.ErrCode())
		assert.Contains(t, appErr2.Error(), "operation failed")
		assert.Contains(t, appErr2.Error(), "failed to connect")
	})

	t.Run("check if error implements error interface", func(t *testing.T) {
		var err error = ledgererr.NewError(ledgererr.CodeInternal, "test", nil)

		assert.NotNil(t, err)
		assert.Equal(t, "test", err.Error())
	})
}

func TestErrorAsInterface(t *testing.T) {
	t.Run("use errors.As to extract AppError", func(t *testing.T) {
		appErr := ledgererr.NewError(ledgererr.CodeDatabaseError, "db error", nil)
		var err error = appErr

		var extractedErr ledgererr.AppError
		assert.True(t, errors.As(err, &extractedErr))
		assert.Equal(t, ledgererr.CodeDatabaseError, extractedErr.Code)
		assert.Equal(t, "db error", extractedErr.Msg)
	})

	t.Run("errors.As returns false for non-AppError", func(t *testing.T) {
		err := errors.New("standard error")

		var appErr ledgererr.AppError
		assert.False(t, errors.As(err, &appErr))
	})
}
