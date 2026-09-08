package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBankAccount_Validate(t *testing.T) {
	tests := []struct {
		name        string
		bankAccount BankAccount
		wantErr     bool
	}{
		{
			name: "valid bank account",
			bankAccount: BankAccount{
				BankCode:      "014",
				AccountNumber: "1234567890",
				AccountName:   "John Doe",
			},
			wantErr: false,
		},
		{
			name: "missing bank code",
			bankAccount: BankAccount{
				BankCode:      "",
				AccountNumber: "1234567890",
				AccountName:   "John Doe",
			},
			wantErr: true,
		},
		{
			name: "missing account number",
			bankAccount: BankAccount{
				BankCode:      "014",
				AccountNumber: "",
				AccountName:   "John Doe",
			},
			wantErr: true,
		},
		{
			name: "missing account name",
			bankAccount: BankAccount{
				BankCode:      "014",
				AccountNumber: "1234567890",
				AccountName:   "",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.bankAccount.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestNewDisbursement(t *testing.T) {
	validBank := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	tests := []struct {
		name        string
		ledgerID    string
		amount      int64
		currency    Currency
		bankAccount BankAccount
		description string
		wantErr     bool
	}{
		{
			name:        "valid disbursement",
			ledgerID:    "ledger-123",
			amount:      100000,
			currency:    CurrencyIDR,
			bankAccount: validBank,
			description: "Withdrawal",
			wantErr:     false,
		},
		{
			name:        "zero amount",
			ledgerID:    "ledger-123",
			amount:      0,
			currency:    CurrencyIDR,
			bankAccount: validBank,
			description: "Withdrawal",
			wantErr:     true,
		},
		{
			name:        "negative amount",
			ledgerID:    "ledger-123",
			amount:      -100000,
			currency:    CurrencyIDR,
			bankAccount: validBank,
			description: "Withdrawal",
			wantErr:     true,
		},
		{
			name:     "invalid bank account",
			ledgerID: "ledger-123",
			amount:   100000,
			currency: CurrencyIDR,
			bankAccount: BankAccount{
				BankCode:      "",
				AccountNumber: "1234567890",
				AccountName:   "John Doe",
			},
			description: "Withdrawal",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDisbursement(tt.ledgerID, tt.amount, tt.currency, tt.bankAccount, tt.description)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, d)
			} else {
				require.NoError(t, err)
				require.NotNil(t, d)
				assert.NotEmpty(t, d.UUID)
				assert.Equal(t, tt.ledgerID, d.LedgerUUID)
				assert.Equal(t, tt.amount, d.Amount)
				assert.Equal(t, tt.currency, d.Currency)
				assert.Equal(t, DisbursementStatusPending, d.Status)
				assert.Equal(t, tt.bankAccount, d.BankAccount)
				assert.Equal(t, tt.description, d.Description)
				assert.NotZero(t, d.CreatedAt)
			}
		})
	}
}

func TestDisbursement_StatusChecks(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	d, err := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
	require.NoError(t, err)

	assert.True(t, d.IsPending())
	assert.False(t, d.IsProcessing())
	assert.False(t, d.IsCompleted())
	assert.False(t, d.IsFailed())
	assert.False(t, d.IsCancelled())
	assert.False(t, d.IsTerminal())
}

func TestDisbursement_CanTransitionTo(t *testing.T) {
	tests := []struct {
		name       string
		fromStatus DisbursementStatus
		toStatus   DisbursementStatus
		canTransit bool
	}{
		{"pending to processing", DisbursementStatusPending, DisbursementStatusProcessing, true},
		{"pending to failed", DisbursementStatusPending, DisbursementStatusFailed, true},
		{"pending to cancelled", DisbursementStatusPending, DisbursementStatusCancelled, true},
		{"pending to completed (immediate success)", DisbursementStatusPending, DisbursementStatusCompleted, true},
		{"processing to completed", DisbursementStatusProcessing, DisbursementStatusCompleted, true},
		{"processing to failed", DisbursementStatusProcessing, DisbursementStatusFailed, true},
		{"processing to pending", DisbursementStatusProcessing, DisbursementStatusPending, false},
		{"processing to cancelled", DisbursementStatusProcessing, DisbursementStatusCancelled, false},
		{"completed to any", DisbursementStatusCompleted, DisbursementStatusPending, false},
		{"failed to any", DisbursementStatusFailed, DisbursementStatusPending, false},
		{"cancelled to any", DisbursementStatusCancelled, DisbursementStatusPending, false},
	}

	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
			d.Status = tt.fromStatus

			canTransit := d.CanTransitionTo(tt.toStatus)
			assert.Equal(t, tt.canTransit, canTransit)
		})
	}
}

func TestDisbursement_MarkProcessing(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	t.Run("valid transition from pending", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

		err := d.MarkProcessing("TX-12345")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusProcessing, d.Status)
		assert.Equal(t, "TX-12345", d.ExternalTransactionID)
	})

	t.Run("invalid transition from completed", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		d.Status = DisbursementStatusCompleted

		err := d.MarkProcessing("TX-12345")
		assert.Error(t, err)
	})
}

func TestDisbursement_MarkCompleted(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	t.Run("valid transition from processing", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		d.Status = DisbursementStatusProcessing

		err := d.MarkCompleted("SP-TX-123")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusCompleted, d.Status)
		assert.Equal(t, "SP-TX-123", d.ExternalTransactionID)
		assert.NotNil(t, d.ProcessedAt)
		assert.True(t, d.IsTerminal())
	})

	t.Run("valid transition from pending (immediate success)", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

		err := d.MarkCompleted("SP-TX-456")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusCompleted, d.Status)
		assert.Equal(t, "SP-TX-456", d.ExternalTransactionID)
		assert.NotNil(t, d.ProcessedAt)
	})
}

func TestDisbursement_MarkFailed(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	t.Run("valid transition from pending", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

		err := d.MarkFailed("gateway API error")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusFailed, d.Status)
		assert.Equal(t, "gateway API error", d.FailureReason)
		assert.NotNil(t, d.ProcessedAt)
		assert.True(t, d.IsTerminal())
	})

	t.Run("valid transition from processing", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		d.Status = DisbursementStatusProcessing
		d.ExternalTransactionID = "TX-12345"

		err := d.MarkFailed("Bank rejected")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusFailed, d.Status)
	})
}

func TestDisbursement_MarkCancelled(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	t.Run("valid transition from pending", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

		err := d.MarkCancelled("User requested cancellation")
		assert.NoError(t, err)
		assert.Equal(t, DisbursementStatusCancelled, d.Status)
		assert.Equal(t, "User requested cancellation", d.FailureReason)
		assert.NotNil(t, d.ProcessedAt)
	})

	t.Run("invalid transition from processing", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		d.Status = DisbursementStatusProcessing

		err := d.MarkCancelled("Cancel")
		assert.Error(t, err)
	})
}

func TestDisbursement_NeedsRollback(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	t.Run("needs rollback when failed without external ID", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		_ = d.MarkFailed("gateway API down")

		assert.True(t, d.NeedsRollback())
	})

	t.Run("no rollback needed when failed with external ID", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")
		_ = d.MarkProcessing("TX-12345")
		_ = d.MarkFailed("Bank rejected")

		assert.False(t, d.NeedsRollback())
	})

	t.Run("no rollback needed when not failed", func(t *testing.T) {
		d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

		assert.False(t, d.NeedsRollback())
	})
}

func TestDisbursement_GetMoney(t *testing.T) {
	bankAccount := BankAccount{
		BankCode:      "014",
		AccountNumber: "1234567890",
		AccountName:   "John Doe",
	}

	d, _ := NewDisbursement("ledger-123", 100000, CurrencyIDR, bankAccount, "Test")

	money := d.GetMoney()
	assert.Equal(t, int64(100000), money.Amount)
	assert.Equal(t, CurrencyIDR, money.Currency)
}
