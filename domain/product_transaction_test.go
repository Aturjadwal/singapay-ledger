package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// redifu.InitRecord allocates every nil pointer field. A new transaction's nullable fields must
// still be nil: Save writes a pointer to the zero time as 0001-01-01, and a pointer to 0 reads
// as a settled fee recorded as zero.
func TestNewProductTransaction_LeavesEveryNullableFieldUnset(t *testing.T) {
	fee, err := NewFeeBreakdown(100_000, 1_000, 700, CurrencyIDR, FeeModelGatewayOnCustomer)
	require.NoError(t, err)

	tx := NewProductTransaction("buyer-1", "seller-1", "product-1", "SERVICE", "INV-1", *fee, nil)

	require.NotNil(t, tx.Record, "the record is initialised")
	assert.NotEmpty(t, tx.UUID)
	assert.Equal(t, TransactionStatusPending, tx.Status)
	assert.Nil(t, tx.CompletedAt)
	assert.Nil(t, tx.SettledAt)
	assert.Nil(t, tx.PlatformFeeTransferredAt)
	assert.Nil(t, tx.SettledPlatformFeeMinor)
	assert.Nil(t, tx.SettledGatewayFeeMinor)
	assert.Nil(t, tx.PlatformResidualMinor)
}
