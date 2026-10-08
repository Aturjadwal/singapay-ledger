package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
)

// ─────────────────────────────────────────────────────────────────────────────
// Settlement — one transaction, on demand.
// ─────────────────────────────────────────────────────────────────────────────
//
// The settling pass works through the open invoices once a day. This is the same work for
// one transaction, when a person asks for it: an operator who wants to know where a payment
// stands at Singapay right now, and to have the ledger follow if it has settled, without
// waiting for the next pass.
//
// It is not a second settlement path. The read is readGatewayTransaction and the write is
// bookSettlement — the pass's own — so a transaction settled here is booked to the sen as
// the pass would have booked it. And the two cannot settle one transaction twice:
// bookSettlement's conditional COMPLETED -> SETTLED move admits exactly one writer, and the
// loser writes nothing.

// SettlementCheckOutcome is what one on-demand settlement check found, and did.
type SettlementCheckOutcome string

const (
	// SettlementCheckSettled: Singapay reports the funds settled, and this check booked the
	// settlement. The transaction moved COMPLETED -> SETTLED and its shares moved from the
	// pending balances to the available ones.
	SettlementCheckSettled SettlementCheckOutcome = "SETTLED"
	// SettlementCheckAlreadySettled: the ledger had already booked the settlement — before
	// the check, or a pass running alongside it got there first. Nothing was written.
	SettlementCheckAlreadySettled SettlementCheckOutcome = "ALREADY_SETTLED"
	// SettlementCheckNotSettled: Singapay has not settled the funds yet. Nothing was
	// written, and the next pass looks at the transaction again.
	SettlementCheckNotSettled SettlementCheckOutcome = "NOT_SETTLED"
	// SettlementCheckBlocked: Singapay settled the funds but took a fee the platform cannot
	// absorb (docs/104-fee-mismatch-reconciliation.md). Left COMPLETED for a person, exactly
	// as the pass leaves it.
	SettlementCheckBlocked SettlementCheckOutcome = "BLOCKED"
	// SettlementCheckNotApplicable: settlement does not apply to the transaction — it is
	// not paid yet (PENDING), or it FAILED or was REFUNDED. Singapay is still asked, so its
	// answer can be put beside the ledger's, but nothing is booked: a settlement is only
	// ever booked on top of the payment the money-in webhook booked.
	SettlementCheckNotApplicable SettlementCheckOutcome = "NOT_APPLICABLE"
)

// GatewaySettlementStatus is what Singapay said about a transaction when it was asked.
type GatewaySettlementStatus struct {
	// Found is false when Singapay holds no payment against the instrument yet: a virtual
	// account nobody has paid into, a payment link with no attempt. For a payment link that
	// is a search's proven answer; a search that could not conclude is an error instead.
	Found bool `json:"found"`
	// Settled is Singapay's has_settle: the funds have reached the merchant balance.
	Settled bool `json:"settled"`
	// SettledAt is Singapay's settle_at; nil when it reported none.
	SettledAt *time.Time `json:"settled_at,omitempty"`
	// Status is Singapay's own status for the payment, as it wrote it.
	Status string `json:"status,omitempty"`

	// GatewayAccountID is the seller's Singapay sub-account the payment was read from.
	GatewayAccountID     string `json:"gateway_account_id"`
	GatewayTransactionID string `json:"gateway_transaction_id,omitempty"`
	MerchantReference    string `json:"merchant_reference,omitempty"`
	PaymentChannel       string `json:"payment_channel,omitempty"`

	// GrossMinor is what the payer paid, in sen. NetMinor and FeeMinor are what reached
	// the merchant and what Singapay kept, in sen — the figures a settlement books, and
	// only meaningful once Settled is true. FeeReported is false for the channels whose fee
	// Singapay never reports (payment links, cards): FeeMinor is then the priced fee.
	GrossMinor  int64 `json:"gross_minor"`
	NetMinor    int64 `json:"net_minor"`
	FeeMinor    int64 `json:"fee_minor"`
	FeeReported bool  `json:"fee_reported"`

	// Raw is the handful of Singapay's own fields a settlement journal records.
	Raw map[string]string `json:"raw,omitempty"`
}

// SettlementCheckResult reports one on-demand settlement check.
type SettlementCheckResult struct {
	ProductTransactionUUID string `json:"product_transaction_uuid"`
	InvoiceNumber          string `json:"invoice_number"`
	PaymentChannel         string `json:"payment_channel"`

	Outcome SettlementCheckOutcome `json:"outcome"`

	// PreviousStatus is the transaction's status when the check began, and Status where it
	// stands after it, read back from the database. They differ on SettlementCheckSettled,
	// or when another writer moved the row while the check ran.
	PreviousStatus domain.TransactionStatus `json:"previous_status"`
	Status         domain.TransactionStatus `json:"status"`
	// SettledAt is the ledger's settled_at, once the transaction is SETTLED.
	SettledAt *time.Time `json:"settled_at,omitempty"`

	// BlockedReason says why a SettlementCheckBlocked settlement could not be booked.
	BlockedReason string `json:"blocked_reason,omitempty"`

	Gateway GatewaySettlementStatus `json:"gateway"`

	CheckedAt time.Time `json:"checked_at"`
}

// CheckTransactionSettlement asks Singapay whether one transaction's funds have settled
// and, when they have and the ledger has not booked it yet, books the settlement — the
// same entries, fee reconciliation and COMPLETED -> SETTLED move the settling pass makes.
//
// Only a COMPLETED transaction is ever settled here. Singapay is asked whatever the
// status, read only, so its answer can be put beside the ledger's: a SETTLED transaction
// Singapay calls unsettled is worth seeing, and so is a PENDING one it calls paid. Neither
// is corrected here — the first would need a reversal and the second the money-in webhook,
// and neither belongs behind a button.
//
// An error means the question could not be answered: the transaction or its instrument is
// unknown, the seller has no Singapay sub-account, Singapay could not be read
// (CodeGatewayAPIError — a payment-link search that ran out of pages or time before it
// could conclude included), or the settlement could not be written. None of those leaves
// anything half-booked; bookSettlement writes in one database transaction.
func (c *LedgerClient) CheckTransactionSettlement(ctx context.Context, productTransactionID string) (*SettlementCheckResult, error) {
	if productTransactionID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "product transaction id is required", nil)
	}

	tx, err := c.repoProvider.ProductTransaction().GetByID(ctx, productTransactionID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrProductTransactionNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the product transaction", err)
	}

	paymentReq, err := c.repoProvider.PaymentRequest().GetByProductTransactionID(ctx, tx.UUID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrPaymentRequestNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the payment request", err)
	}

	sellerAccount, err := c.repoProvider.Account().GetByID(ctx, tx.SellerAccountID)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the seller account", err)
	}
	if sellerAccount.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			fmt.Sprintf("seller account %s has no Singapay sub-account id, so Singapay cannot be asked about its payments", tx.SellerAccountID), nil)
	}

	result := &SettlementCheckResult{
		ProductTransactionUUID: tx.UUID,
		InvoiceNumber:          tx.InvoiceNumber,
		PaymentChannel:         paymentReq.PaymentChannel,
		PreviousStatus:         tx.Status,
		Status:                 tx.Status,
		SettledAt:              recordedTime(tx.SettledAt),
		CheckedAt:              time.Now(),
	}

	reading, err := c.readGatewayTransaction(ctx, sellerAccount.SingapayAccountID, tx, paymentReq)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "could not read the transaction back from Singapay", err)
	}
	result.Gateway = gatewaySettlementStatus(sellerAccount.SingapayAccountID, paymentReq, reading)

	switch {
	case tx.IsSettled():
		result.Outcome = SettlementCheckAlreadySettled
	case !tx.IsCompleted():
		result.Outcome = SettlementCheckNotApplicable
	case !reading.Found || !reading.HasSettle:
		result.Outcome = SettlementCheckNotSettled
	default:
		if err := c.settleChecked(ctx, tx, reading, result); err != nil {
			return nil, err
		}
	}

	c.logger.InfoContext(ctx, "Checked a transaction's settlement on demand",
		"product_transaction_uuid", tx.UUID,
		"invoice_number", tx.InvoiceNumber,
		"payment_channel", paymentReq.PaymentChannel,
		"outcome", string(result.Outcome),
		"previous_status", string(result.PreviousStatus),
		"status", string(result.Status),
		"gateway_found", reading.Found,
		"gateway_settled", reading.HasSettle,
		"gateway_status", reading.Status,
	)

	return result, nil
}

// settleChecked books a settlement Singapay has confirmed, through the pass's own
// bookSettlement, and records on result what happened.
func (c *LedgerClient) settleChecked(ctx context.Context, tx *domain.ProductTransaction, reading *gatewayReading, result *SettlementCheckResult) error {
	outcome, err := c.bookSettlement(ctx, tx, reading.Settled)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to book the settlement", err)
	}

	switch outcome {
	case settleOutcomeSettled:
		result.Outcome = SettlementCheckSettled
	case settleOutcomeAlreadySettled:
		result.Outcome = SettlementCheckAlreadySettled
	case settleOutcomeBlocked:
		result.Outcome = SettlementCheckBlocked
		// resolveFeeAdjustment is pure, so asking it again gives the reason bookSettlement
		// logged and did not return.
		_, result.BlockedReason = resolveFeeAdjustment(tx, reading.Settled)
	default:
		result.Outcome = SettlementCheckNotSettled
	}

	// The row is the answer, not the in-memory copy (AGENTS.md §4): read back where the
	// transaction now stands rather than assuming what the outcome implies.
	fresh, err := c.repoProvider.ProductTransaction().GetByID(ctx, tx.UUID)
	if err != nil {
		// The settlement is committed either way; the response just cannot confirm it.
		c.logger.WarnContext(ctx, "Could not read a transaction back after checking its settlement",
			"product_transaction_uuid", tx.UUID,
			"error", err,
		)
		return nil
	}
	result.Status = fresh.Status
	result.SettledAt = recordedTime(fresh.SettledAt)

	return nil
}

// recordedTime is t, or nil when it was never set. The repository reads a NULL timestamp
// back as a pointer to the zero time — redifu.InitRecord allocates every nil pointer the
// scan leaves — and a result must not report 0001-01-01 as the day something settled.
func recordedTime(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
}

func gatewaySettlementStatus(gatewayAccountID string, paymentReq *domain.PaymentRequest, reading *gatewayReading) GatewaySettlementStatus {
	status := GatewaySettlementStatus{
		Found:            reading.Found,
		Settled:          reading.HasSettle,
		SettledAt:        reading.SettleAt,
		Status:           reading.Status,
		GatewayAccountID: gatewayAccountID,
		PaymentChannel:   paymentReq.PaymentChannel,
	}
	if !reading.Found {
		return status
	}

	settled := reading.Settled
	status.GatewayTransactionID = settled.GatewayTransactionID
	status.MerchantReference = settled.MerchantReference
	status.PaymentChannel = settled.PaymentChannel
	status.GrossMinor = settled.GrossMinor
	status.FeeReported = settled.FeeReported
	status.Raw = settled.Raw
	// Before settlement a QRIS row has no settled-to-merchant amount yet, so a net and a
	// fee derived from it would read as "Singapay kept everything". Leave them zero until
	// they mean something.
	if reading.HasSettle {
		status.NetMinor = settled.NetMinor
		status.FeeMinor = settled.FeeMinor
	}
	return status
}
