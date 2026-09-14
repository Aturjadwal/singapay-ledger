package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Settlement — the per-transaction path.
// ─────────────────────────────────────────────────────────────────────────────
//
// Singapay publishes no settlement file, and its settlement webhook carries totals and a
// date window but no list of the transactions a batch covered. There are two ways to close
// that gap. This is the second one.
//
// The first — replay the window against the per-product transaction lists — is described
// in docs/102-settlement-reconciliation.md, and it is blocked on two questions that cannot
// be answered from the documentation: what timezone Singapay's offsetless window text is
// in, and which of a QRIS transaction's two settlement timestamps the window keys on. Both
// have wrong answers that produce plausible-looking results.
//
// This path does not ask either question. It treats the webhook as a doorbell:
//
//	the webhook says "something settled"     -> stored, verbatim, nothing booked
//	the ledger says "these invoices are open" -> GetAwaitingSettlement
//	Singapay is asked about each one directly -> has_settle, and the fee it actually took
//
// No window is ever used to select rows, so no window can be misread. The cost is one call
// per open invoice instead of one call per account per product — bounded by work that is
// shrinking rather than by the size of the merchant.
//
// The webhook is therefore an optimisation for latency, not a correctness dependency: a
// delivery that never arrives delays settlement to the next floor-age tick rather than
// losing it. See ProcessSettlementNotifications.

// SettlementFloorAge is how stale an unsettled transaction may get before a pass runs even
// though no settlement webhook arrived.
//
// It exists because "only act on a notification" reintroduces the failure this design is
// meant to remove: if a delivery is lost — an IP allowlist change, a deploy window,
// Singapay's retries exhausted — nothing would ever run again and transactions would sit in
// COMPLETED indefinitely, with no error anywhere.
//
// 24 hours clears the T+1 cycle with room to spare, so on a healthy system the floor never
// fires and the notification is what drives every pass.
const SettlementFloorAge = 24 * time.Hour

// defaultSettlementBatchSize bounds one pass. A pass is resumable by construction — the
// work set is "what is still COMPLETED", so whatever a truncated pass did not reach is
// simply still there on the next tick, oldest first.
const defaultSettlementBatchSize = 200

// HandleSettlementNotification stores a settlement webhook and books nothing.
//
// That is the whole job. The payload announces a batch with totals and a window and no
// list of what it covered, so there is nothing here that could justify a ledger entry;
// what there is, is a signal that it is worth asking Singapay about the open invoices, and
// a record of exactly what Singapay said for when a settled amount is argued about later.
//
// Refund events are stored for review rather than acted on. settlement.refunded can pull
// back funds that are already AVAILABLE and may already have been withdrawn, and this
// ledger has no negative-balance policy. Inventing one inside a webhook handler is the
// wrong place to decide it.
//
// A redelivery is ordinary traffic: Singapay retries, the (settlement_id, event) identity
// absorbs it, and the caller is told success either way. Answering anything else teaches
// Singapay to keep retrying a delivery that was already accepted.
func (c *LedgerClient) HandleSettlementNotification(ctx context.Context, req singapay.WebhookRequest) error {
	if err := c.gateway.VerifyWebhook(req); err != nil {
		c.logger.WarnContext(ctx, "Rejected a settlement webhook that failed signature verification",
			"endpoint", req.Endpoint,
			"error", err,
		)
		return ledgererr.ErrWebhookVerificationFailed.WithError(err)
	}

	notification, err := singapay.ParseSettlementNotification(req.Body)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "could not parse settlement notification", err)
	}

	settlement := notification.Data.Settlement
	settlementID := strconv.FormatInt(settlement.ID, 10)

	record, err := domain.NewSettlementNotification(
		settlementID,
		settlement.ReferenceNo,
		string(notification.Event),
		req.Body,
	)
	if err != nil {
		return err
	}

	record.Method = string(settlement.Method)
	record.Type = settlement.Type
	record.Currency = settlement.Currency
	record.TotalTransactions = notification.Data.TotalTransactions
	record.Amount = settlement.Amount.Minor()
	record.TotalFee = settlement.TotalAdminFee.Minor() +
		settlement.TotalVendorFee.Minor() +
		settlement.TotalOurMargin.Minor()

	// Stored for audit only. The settling pass never filters on these: Singapay writes
	// them without an offset, so their real boundaries are an assumption, and the
	// per-transaction path has no use for them.
	if settlement.StartDate.Set {
		start := settlement.StartDate.Time
		record.StartDate = &start
	}
	if settlement.EndDate.Set {
		end := settlement.EndDate.Time
		record.EndDate = &end
	}

	// Everything that is not a completed balance settlement is parked for a person.
	//
	// Refunds need a negative-balance policy that does not exist. A bank-account or
	// e-wallet settlement paid out to a nominated account instead of moving PENDING into
	// AVAILABLE, so a settling pass triggered by one would look for work that is not
	// there — harmless, but recording it as PROCESSED would claim something untrue.
	switch {
	case notification.Event != singapay.EventSettlementCompleted:
		record.Status = domain.SettlementNotificationNeedsReview
		record.FailureReason = "refund event: needs a negative-balance policy before it can be booked automatically"
	case !settlement.MovesPendingToAvailable():
		record.Status = domain.SettlementNotificationNeedsReview
		record.FailureReason = fmt.Sprintf("settlement_method %q pays out rather than moving PENDING to AVAILABLE", settlement.Method)
	}

	stored, err := c.repoProvider.SettlementNotification().Save(ctx, record)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to store settlement notification", err)
	}

	if !stored {
		c.logger.InfoContext(ctx, "Settlement webhook redelivered; already stored",
			"settlement_id", settlementID,
			"settlement_reference", settlement.ReferenceNo,
			"event", string(notification.Event),
		)
		return nil
	}

	c.logger.InfoContext(ctx, "Stored settlement notification",
		"settlement_id", settlementID,
		"settlement_reference", settlement.ReferenceNo,
		"event", string(notification.Event),
		"settlement_method", string(settlement.Method),
		"settlement_type", settlement.Type,
		"total_transactions", notification.Data.TotalTransactions,
		"status", string(record.Status),
	)

	return nil
}

// SettlementPassResult reports what one pass did.
type SettlementPassResult struct {
	// Triggered says whether the pass looked at transactions at all. False means the
	// tick found no notification and no transaction old enough to justify a sweep — the
	// ordinary quiet tick, and not a failure.
	Triggered bool `json:"triggered"`
	// TriggerReason is "notification", "floor_age", or "" when not triggered.
	TriggerReason string `json:"trigger_reason,omitempty"`

	NotificationsClaimed int `json:"notifications_claimed"`

	Examined  int `json:"examined"`
	Settled   int `json:"settled"`
	StillOpen int `json:"still_open"`
	Blocked   int `json:"blocked"`
	Failed    int `json:"failed"`

	// OldestAwaitingAge is how long the oldest unsettled transaction has been waiting at
	// the end of the pass. This is the number worth alarming on: it climbs monotonically
	// whenever settlement stops working, including when the pass itself runs cleanly and
	// books nothing.
	OldestAwaitingAge time.Duration `json:"oldest_awaiting_age"`

	Errors []SettlementError `json:"errors,omitempty"`
}

// SettlementError is one transaction the pass could not finish.
type SettlementError struct {
	ProductTransactionUUID string `json:"product_transaction_uuid"`
	InvoiceNumber          string `json:"invoice_number"`
	Reason                 string `json:"reason"`
}

// ProcessSettlementNotifications runs one settlement pass.
//
// This is the worker's entry point, and it is safe to call on a schedule forever: on a
// quiet tick it runs two cheap reads and returns. It triggers when there is a stored
// notification to act on, OR when the oldest unsettled transaction has been waiting longer
// than SettlementFloorAge — the second condition is what keeps a lost webhook from
// stranding money.
//
// batchSize bounds how many transactions one pass examines. Passing 0 uses the default.
func (c *LedgerClient) ProcessSettlementNotifications(ctx context.Context, batchSize int) (*SettlementPassResult, error) {
	if batchSize <= 0 {
		batchSize = defaultSettlementBatchSize
	}

	result := &SettlementPassResult{Errors: []SettlementError{}}

	notifications, err := c.repoProvider.SettlementNotification().GetActionable(ctx, 50)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read settlement notifications", err)
	}

	oldest, hasOpen, err := c.repoProvider.ProductTransaction().OldestAwaitingSettlement(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the oldest unsettled transaction", err)
	}
	if hasOpen {
		result.OldestAwaitingAge = time.Since(oldest)
	}

	switch {
	case len(notifications) > 0:
		result.TriggerReason = "notification"
	case hasOpen && time.Since(oldest) >= SettlementFloorAge:
		// No webhook arrived, and money has been waiting too long for that to be
		// explained by a settlement cycle. Sweep anyway.
		result.TriggerReason = "floor_age"
		c.logger.WarnContext(ctx, "Settlement sweep triggered by age, not by a notification — a settlement webhook may have been missed",
			"oldest_awaiting_age", result.OldestAwaitingAge.String(),
			"floor_age", SettlementFloorAge.String(),
		)
	default:
		return result, nil
	}

	result.Triggered = true

	// Claim first. A claim that loses means another worker is already on this row, and
	// the right response is to leave it alone rather than to duplicate the pass.
	claimed := make([]*domain.SettlementNotification, 0, len(notifications))
	for _, n := range notifications {
		got, err := c.repoProvider.SettlementNotification().ClaimIfActionable(ctx, n.UUID)
		if err != nil {
			c.logger.ErrorContext(ctx, "Failed to claim a settlement notification",
				"settlement_notification_uuid", n.UUID,
				"error", err,
			)
			continue
		}
		if got {
			claimed = append(claimed, n)
		}
	}
	result.NotificationsClaimed = len(claimed)

	transactions, err := c.repoProvider.ProductTransaction().GetAwaitingSettlement(ctx, batchSize)
	if err != nil {
		c.releaseClaims(ctx, claimed, "failed to read transactions awaiting settlement")
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read transactions awaiting settlement", err)
	}

	for _, tx := range transactions {
		result.Examined++

		outcome, err := c.settleOne(ctx, tx)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, SettlementError{
				ProductTransactionUUID: tx.UUID,
				InvoiceNumber:          tx.InvoiceNumber,
				Reason:                 err.Error(),
			})
			c.logger.ErrorContext(ctx, "Settlement failed for a transaction",
				"product_transaction_uuid", tx.UUID,
				"invoice_number", tx.InvoiceNumber,
				"error", err,
			)
			continue
		}

		switch outcome {
		case settleOutcomeSettled:
			result.Settled++
		case settleOutcomeNotYet:
			result.StillOpen++
		case settleOutcomeBlocked:
			result.Blocked++
		}
	}

	// A notification is PROCESSED once a pass has run after seeing it. That is all it
	// claims — whether any given transaction settled is recorded on that transaction.
	// Keeping the two apart is deliberate: a partial pass must not be able to record
	// itself as a complete one.
	for _, n := range claimed {
		if err := c.repoProvider.SettlementNotification().MarkProcessed(ctx, n.UUID); err != nil {
			c.logger.ErrorContext(ctx, "Failed to mark a settlement notification processed",
				"settlement_notification_uuid", n.UUID,
				"error", err,
			)
		}
	}

	// Re-read after the pass: this is the number to alarm on, and it is only meaningful
	// once the pass has had its chance to move things.
	oldest, hasOpen, err = c.repoProvider.ProductTransaction().OldestAwaitingSettlement(ctx)
	if err == nil && hasOpen {
		result.OldestAwaitingAge = time.Since(oldest)
	} else if err == nil {
		result.OldestAwaitingAge = 0
	}

	c.logger.InfoContext(ctx, "Settlement pass finished",
		"trigger", result.TriggerReason,
		"notifications_claimed", result.NotificationsClaimed,
		"examined", result.Examined,
		"settled", result.Settled,
		"still_open", result.StillOpen,
		"blocked", result.Blocked,
		"failed", result.Failed,
		"oldest_awaiting_age", result.OldestAwaitingAge.String(),
	)

	return result, nil
}

func (c *LedgerClient) releaseClaims(ctx context.Context, claimed []*domain.SettlementNotification, reason string) {
	for _, n := range claimed {
		if err := c.repoProvider.SettlementNotification().MarkFailed(ctx, n.UUID, reason); err != nil {
			c.logger.ErrorContext(ctx, "Failed to release a settlement notification claim",
				"settlement_notification_uuid", n.UUID,
				"error", err,
			)
		}
	}
}

type settleOutcome int

const (
	settleOutcomeNotYet settleOutcome = iota
	settleOutcomeSettled
	settleOutcomeBlocked
)

// settleOne asks Singapay about one open invoice and, if the funds have settled, books it.
//
// The read is a point lookup, not a search: the channel says which endpoint, and the
// payment request says which key. Nothing about a settlement batch enters into it, which
// is what makes the result independent of how the batch's window is interpreted.
func (c *LedgerClient) settleOne(ctx context.Context, tx *domain.ProductTransaction) (settleOutcome, error) {
	paymentReq, err := c.repoProvider.PaymentRequest().GetByProductTransactionID(ctx, tx.UUID)
	if err != nil {
		return settleOutcomeNotYet, fmt.Errorf("failed to read the payment request: %w", err)
	}

	sellerAccount, err := c.repoProvider.Account().GetByID(ctx, tx.SellerAccountID)
	if err != nil {
		return settleOutcomeNotYet, fmt.Errorf("failed to read the seller account: %w", err)
	}
	if sellerAccount.SingapayAccountID == "" {
		return settleOutcomeNotYet, fmt.Errorf("seller account %s has no Singapay sub-account id", tx.SellerAccountID)
	}

	settled, err := c.readSettledTransaction(ctx, sellerAccount.SingapayAccountID, tx, paymentReq)
	if err != nil {
		return settleOutcomeNotYet, err
	}
	if settled == nil {
		// Not settled yet. The ordinary answer for most of a pass, and not worth a log
		// line per transaction — the pass reports the count.
		return settleOutcomeNotYet, nil
	}

	return c.bookSettlement(ctx, tx, *settled)
}

// readSettledTransaction returns the settled gateway row for a transaction, or nil when
// the funds have not settled yet.
//
// Key selection is the interesting part. GatewayTransactionID and GatewayTransactionRef
// come from the money-in webhook and are the direct keys; RequestID and PaymentCode, from
// instrument creation, are the fallbacks for rows that predate those columns. The fallback
// differs per channel because the instrument and the transaction are the same entity for
// QRIS and e-wallet and different entities for VA and payment link.
func (c *LedgerClient) readSettledTransaction(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
) (*domain.SettledTransaction, error) {
	switch paymentChannelKind(paymentReq.PaymentChannel) {

	case channelVirtualAccount:
		va, err := c.readVATransaction(ctx, gatewayAccountID, paymentReq)
		if err != nil {
			return nil, err
		}
		if va == nil || !va.HasSettle {
			return nil, nil
		}
		// The VA fee is a single reported figure, so net is derived rather than read.
		gross := va.Amount.Minor()
		fee := va.Fees.Amount.Minor()
		return &domain.SettledTransaction{
			MerchantReference:    va.MerchantReffNo,
			GatewayTransactionID: va.TransactionID,
			GatewayAccountID:     gatewayAccountID,
			PaymentChannel:       paymentReq.PaymentChannel,
			GrossMinor:           gross,
			NetMinor:             gross - fee,
			FeeMinor:             fee,
			FeeReported:          true,
			Raw: map[string]string{
				"transaction_id": va.TransactionID,
				"va_number":      va.VANumber,
				"status":         string(va.Status),
			},
		}, nil

	case channelQRIS:
		id, err := gatewayNumericID(paymentReq)
		if err != nil {
			return nil, err
		}
		qr, err := c.gateway.GetQRISTransaction(ctx, gatewayAccountID, id)
		if err != nil {
			return nil, fmt.Errorf("failed to read the QRIS transaction: %w", err)
		}
		if !qr.HasSettle {
			return nil, nil
		}
		// Derive the fee from the net rather than by summing MDRCost, VendorFee and
		// OurMargin. How those three relate to the net is not documented, and Singapay's
		// own field examples do not add up to it — 150 + 50 against a 250 difference. The
		// net is the figure that decides what the seller actually received, so it is the
		// one to trust until the decomposition is confirmed against a sandbox.
		gross := qr.TotalAmount.Minor()
		net := qr.SettledToMerchant.Minor()
		return &domain.SettledTransaction{
			MerchantReference:    qr.MerchantReffNo,
			GatewayTransactionID: strconv.FormatInt(qr.ID, 10),
			GatewayAccountID:     gatewayAccountID,
			PaymentChannel:       paymentReq.PaymentChannel,
			GrossMinor:           gross,
			NetMinor:             net,
			FeeMinor:             gross - net,
			FeeReported:          true,
			Raw: map[string]string{
				"reff_no":    qr.ReffNo,
				"status":     string(qr.Status),
				"mdr_cost":   strconv.FormatInt(qr.MDRCost.Minor(), 10),
				"vendor_fee": strconv.FormatInt(qr.VendorFee.Minor(), 10),
				"our_margin": strconv.FormatInt(qr.OurMargin.Minor(), 10),
			},
		}, nil

	case channelEwallet:
		key := paymentReq.GatewayTransactionID
		if key == "" {
			key = paymentReq.RequestID
		}
		if key == "" {
			return nil, fmt.Errorf("no gateway identifier stored for e-wallet transaction %s", tx.InvoiceNumber)
		}
		ew, err := c.gateway.GetEwalletTransaction(ctx, gatewayAccountID, key)
		if err != nil {
			return nil, fmt.Errorf("failed to read the e-wallet transaction: %w", err)
		}
		if !ew.HasSettle {
			return nil, nil
		}
		return &domain.SettledTransaction{
			MerchantReference:    ew.MerchantReffNo,
			GatewayTransactionID: strconv.FormatInt(ew.ID, 10),
			GatewayAccountID:     gatewayAccountID,
			PaymentChannel:       paymentReq.PaymentChannel,
			GrossMinor:           ew.TotalAmount.Minor(),
			NetMinor:             ew.NetAmount.Minor(),
			FeeMinor:             ew.MerchantFee.Minor(),
			FeeReported:          true,
			Raw: map[string]string{
				"reff_no": ew.ReffNo,
				"status":  string(ew.Status),
				"vendor":  ew.Vendor,
			},
		}, nil

	case channelPaymentLink:
		history, err := c.readPaymentLinkHistory(ctx, gatewayAccountID, tx, paymentReq)
		if err != nil {
			return nil, err
		}
		if history == nil || !history.HasSettle {
			return nil, nil
		}
		// A payment link reports no fee anywhere — not in the list, not in the detail.
		// The expected fee is used so the delta is zero by construction, and FeeReported
		// records that this is the absence of a reconciliation rather than a clean one.
		//
		// The expected fee is whole rupiah and this struct is sen, so it is converted here
		// like every other figure crossing that boundary — a delta that is zero by
		// construction is only zero if both sides are counted in the same unit.
		gross := history.Amount.Minor()
		feeMinor := domain.RupiahToMinor(tx.Fee.GatewayFee)
		return &domain.SettledTransaction{
			MerchantReference:    tx.InvoiceNumber,
			GatewayTransactionID: strconv.FormatInt(history.ID, 10),
			GatewayAccountID:     gatewayAccountID,
			PaymentChannel:       ChannelPaymentLink,
			GrossMinor:           gross,
			NetMinor:             gross - feeMinor,
			FeeMinor:             feeMinor,
			FeeReported:          false,
			Raw: map[string]string{
				"reff_no":              history.ReffNo,
				"payment_link_reff_no": history.PaymentLinkReffNo,
				"status":               string(history.Status),
			},
		}, nil

	default:
		return nil, fmt.Errorf("payment channel %q is not a Singapay money-in product", paymentReq.PaymentChannel)
	}
}

// readVATransaction reads the VA payment for a transaction.
//
// Two routes, and the fallback is the common one for anything created before the gateway
// identifiers were persisted. GatewayTransactionRef is the VA transaction's business id
// ("VA-20251024-0001H9X8ZK"), which only the money-in webhook ever reports. Without it the
// VA number is the key, and the collection it returns must hold exactly one row: these VAs
// are issued temporary, closed and single-use, so more than one payment against one of
// them means the VA did not come from this code path and should not be settled blind.
func (c *LedgerClient) readVATransaction(
	ctx context.Context,
	gatewayAccountID string,
	paymentReq *domain.PaymentRequest,
) (*singapay.VATransaction, error) {
	if paymentReq.GatewayTransactionRef != "" {
		va, err := c.gateway.GetVATransaction(ctx, gatewayAccountID, paymentReq.GatewayTransactionRef)
		if err != nil {
			return nil, fmt.Errorf("failed to read the VA transaction: %w", err)
		}
		return va, nil
	}

	if paymentReq.PaymentCode == "" {
		return nil, fmt.Errorf("no VA number stored for payment request %s", paymentReq.UUID)
	}

	rows, _, err := c.gateway.GetVATransactionsByVANumber(ctx, gatewayAccountID, paymentReq.PaymentCode)
	if err != nil {
		return nil, fmt.Errorf("failed to read VA transactions by VA number: %w", err)
	}
	switch len(rows) {
	case 0:
		// Nobody has paid into it yet, or the payment has not posted. Not an error.
		return nil, nil
	case 1:
		return &rows[0], nil
	default:
		return nil, fmt.Errorf(
			"virtual account %s has %d transactions; expected one for a single-use VA — refusing to guess which settled",
			paymentReq.PaymentCode, len(rows))
	}
}

// readPaymentLinkHistory reads the settled attempt against a payment link.
//
// The direct route needs payment_link_histories.id, which is the id of one ATTEMPT and is
// only known from the money-in webhook. What creation returns is the LINK's id, which this
// endpoint does not accept — so without a stored attempt id the only route is to list the
// account's history and match on the reference we set at creation.
func (c *LedgerClient) readPaymentLinkHistory(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
) (*singapay.PaymentLinkHistory, error) {
	if paymentReq.GatewayTransactionID != "" {
		id, err := strconv.ParseInt(paymentReq.GatewayTransactionID, 10, 64)
		if err == nil {
			history, err := c.gateway.GetPaymentLinkHistory(ctx, gatewayAccountID, id)
			if err != nil {
				return nil, fmt.Errorf("failed to read the payment link history: %w", err)
			}
			return history, nil
		}
	}

	rows, _, err := c.gateway.ListPaymentLinkHistories(ctx, gatewayAccountID, singapay.SettlementWindow{
		ReffNo:  tx.InvoiceNumber,
		PerPage: 25,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list payment link histories: %w", err)
	}

	// ReffNo matches partially, so an exact comparison still has to be made here.
	for i := range rows {
		if rows[i].PaymentLinkReffNo == tx.InvoiceNumber || rows[i].ReffNo == tx.InvoiceNumber {
			return &rows[i], nil
		}
	}

	return nil, nil
}

// gatewayNumericID resolves the numeric transaction id for the channels whose detail
// endpoint takes one, preferring the webhook-sourced value over the creation-time one.
func gatewayNumericID(paymentReq *domain.PaymentRequest) (int64, error) {
	raw := paymentReq.GatewayTransactionID
	if raw == "" {
		raw = paymentReq.RequestID
	}
	if raw == "" {
		return 0, fmt.Errorf("no gateway identifier stored for payment request %s", paymentReq.UUID)
	}

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gateway identifier %q is not numeric: %w", raw, err)
	}

	return id, nil
}

// bookSettlement writes the ledger entries for one settled transaction.
//
// Everything here happens in a single database transaction whose first statement is the
// conditional COMPLETED -> SETTLED move. That ordering is the point: the conditional update
// takes the row lock, and a caller that finds the row already moved rolls back having
// written nothing. Two passes racing on one transaction therefore produce one settlement,
// not two sets of insert-only entries that would need an audit to unpick.
//
// The fee rules are docs/104-fee-mismatch-reconciliation.md and resolveFeeAdjustment. In
// short: the seller is paid what they were priced, the platform sub-account balances the
// difference between the quoted gateway fee and the real one in both directions, and when
// absorbing it would drive the platform negative the transaction is left alone for a person
// to look at.
func (c *LedgerClient) bookSettlement(
	ctx context.Context,
	tx *domain.ProductTransaction,
	settled domain.SettledTransaction,
) (settleOutcome, error) {
	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		return settleOutcomeNotYet, fmt.Errorf("failed to read the platform account: %w", err)
	}

	gatewayAccount, err := c.repoProvider.Account().GetPaymentGatewayAccount(ctx)
	if err != nil {
		return settleOutcomeNotYet, fmt.Errorf("failed to read the payment gateway account: %w", err)
	}

	adjustment, blocked := resolveFeeAdjustment(tx, settled)
	if blocked != "" {
		c.logger.ErrorContext(ctx, "Settlement blocked: the gateway fee cannot be absorbed",
			"product_transaction_uuid", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
			"fee_model", string(tx.Fee.FeeModel),
			"estimated_gateway_fee", tx.Fee.GatewayFee,
			"actual_gateway_fee", formatMinor(settled.FeeMinor),
			"fee_delta", formatMinor(adjustment.FeeDeltaMinor),
			"reason", blocked,
		)
		// Deliberately left in COMPLETED. It stays visible in GetAwaitingSettlement and
		// keeps pushing up the oldest-awaiting age, which is exactly the behaviour wanted
		// for something a person has to decide.
		return settleOutcomeBlocked, nil
	}

	journal := domain.NewJournal(
		domain.EventTypeSettlement,
		domain.SourceTypeProductTransaction,
		tx.UUID,
		map[string]any{
			"invoice_number":         tx.InvoiceNumber,
			"payment_channel":        settled.PaymentChannel,
			"gateway_transaction_id": settled.GatewayTransactionID,
			"gateway_account_id":     settled.GatewayAccountID,
			// The _minor figures are sen and are the authoritative ones; their rupiah
			// siblings are what the ledger entries could express. Where they disagree the
			// difference is platform_residual_minor, and it is the platform's, never the
			// seller's.
			"gross_amount_minor":         settled.GrossMinor,
			"net_amount_minor":           settled.NetMinor,
			"estimated_gateway_fee":      tx.Fee.GatewayFee,
			"actual_gateway_fee_minor":   settled.FeeMinor,
			"fee_reported":               settled.FeeReported,
			"fee_delta_minor":            adjustment.FeeDeltaMinor,
			"fee_model":                  string(tx.Fee.FeeModel),
			"priced_platform_fee":        tx.Fee.PlatformFee,
			"settled_platform_fee_minor": adjustment.PlatformFeeMinor,
			"platform_residual_minor":    adjustment.PlatformResidualMinor(),
			"priced_seller_net":          tx.Fee.SellerNetAmount,
			"settled_seller_net":         adjustment.SellerNet,
			"raw_gateway_data":           settled.Raw,
		},
	)

	// The seller's two legs are equal, and that equality is the rule: a seller clears the
	// PENDING they were priced and receives exactly that amount, whatever Singapay charged.
	entries := domain.NewSettlementEntriesForAccount(journal.UUID, tx.UUID, tx.SellerAccountID, adjustment.SellerNet)

	// The platform's are not. Its PENDING was credited the priced fee at payment time and
	// clears by precisely that; what it receives is the balanced figure. The gap between
	// the two legs IS the fee delta — there is no separate write-off or surplus entry any
	// more, because booking the delta twice was what made the old pair of entries need one.
	entries = append(entries, domain.NewSettlementEntriesForAccountSplit(
		journal.UUID, tx.UUID, platformAccount.UUID, tx.Fee.PlatformFee, adjustment.PlatformFeeRupiah())...)

	// The gateway expense account clears the fee it was credited at payment time. The
	// difference between that and what Singapay really took is carried by the platform's
	// two legs above, not by varying this one — otherwise the same delta would be counted
	// in two places.
	entries = append(entries, domain.NewGatewayFeeSettlementEntry(journal.UUID, tx.UUID, gatewayAccount.UUID, tx.Fee.GatewayFee))

	settledAt := time.Now()

	err = c.txProvider.Transact(ctx, func(dbtx repo.Tx) error {
		moved, err := dbtx.ProductTransaction().UpdateStatusIf(ctx, tx.UUID,
			domain.TransactionStatusCompleted, domain.TransactionStatusSettled, settledAt)
		if err != nil {
			return err
		}
		if !moved {
			return errAlreadySettled
		}

		// Written inside the same transaction as the status move, so the transfer step
		// can never find a SETTLED row whose figures are still unset.
		if err := dbtx.ProductTransaction().SaveSettledFees(ctx, tx.UUID,
			adjustment.PlatformFeeMinor, settled.FeeMinor, adjustment.PlatformResidualMinor()); err != nil {
			return err
		}

		if err := dbtx.Journal().Save(ctx, journal); err != nil {
			return err
		}

		return dbtx.LedgerEntry().SaveBatch(ctx, entries)
	})

	if errors.Is(err, errAlreadySettled) {
		// Another pass got there first. Nothing was written here, and the caller's
		// question — is this settled? — is answered yes.
		c.logger.InfoContext(ctx, "Transaction was already settled by another pass",
			"product_transaction_uuid", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
		)
		return settleOutcomeSettled, nil
	}
	if err != nil {
		return settleOutcomeNotYet, fmt.Errorf("failed to write the settlement: %w", err)
	}

	c.logger.InfoContext(ctx, "Settled a transaction",
		"product_transaction_uuid", tx.UUID,
		"invoice_number", tx.InvoiceNumber,
		"payment_channel", settled.PaymentChannel,
		"seller_net", adjustment.SellerNet,
		"platform_fee", formatMinor(adjustment.PlatformFeeMinor),
		"platform_residual_minor", adjustment.PlatformResidualMinor(),
		"actual_gateway_fee", formatMinor(settled.FeeMinor),
		"fee_delta", formatMinor(adjustment.FeeDeltaMinor),
		"fee_reported", settled.FeeReported,
	)

	return settleOutcomeSettled, nil
}

// errAlreadySettled marks the losing side of a race on one transaction. It never leaves
// bookSettlement.
var errAlreadySettled = errors.New("transaction already settled")

// feeAdjustment is what each party actually receives once the real gateway fee is known.
//
// # The rule
//
// The seller is paid what they were priced, always. Singapay's money-in fee is a decimal
// figure and the one quoted at checkout is an estimate from fee_configs, so the two rarely
// agree to the sen — but the difference is not the seller's to carry in either direction,
// and a net that moves by a few sen per transaction is a net nobody can reconcile against
// what the checkout promised. The platform sub-account is the balancing account:
//
//	platformAdjustment = estimatedGatewayFee - actualGatewayFee
//
// Positive — Singapay charged less than estimated — and the residual is the platform's.
// Negative and the platform absorbs the shortfall out of its own fee. Zero and nothing
// moves. This holds the invariant the whole design exists for: a seller's Singapay
// sub-account balance and their ledger balance are the same number, because the only two
// things that leave that sub-account are the fee Singapay itself deducts and the platform
// fee swept out by ProcessPlatformFeeTransfer, and the sweep moves precisely the figure
// below.
//
// # Units
//
// Everything here is sen. The delta is routinely a fraction of a rupiah — the case this
// whole mechanism exists for — so the arithmetic cannot be done in whole rupiah without
// throwing away the very quantity it is meant to place. PlatformFeeMinor is the one figure
// that can carry a fraction; SellerNet is whole rupiah because the priced net always is.
type feeAdjustment struct {
	// FeeDeltaMinor is actual minus estimated, in sen. Positive means Singapay took more
	// than was quoted at checkout.
	FeeDeltaMinor int64

	// SellerNet is what moves from PENDING to AVAILABLE for the seller, in whole rupiah.
	// It is the priced figure, unconditionally — the field exists to be carried into the
	// journal and the entries, not to be adjusted.
	SellerNet int64

	// PlatformFeeMinor is what the platform actually earned, in sen: the priced platform
	// fee less the delta. It is what ProcessPlatformFeeTransfer moves, and the account
	// transfer endpoint takes a decimal amount, so it moves exactly — fraction included.
	PlatformFeeMinor int64
}

// PlatformFeeRupiah is PlatformFeeMinor truncated to whole rupiah, for the ledger entry.
//
// ledger_entries.amount is whole rupiah and stays that way; the sen that will not divide
// are returned by PlatformResidualMinor and persisted alongside the transaction instead of
// being dropped. The platform's ledger balance therefore trails its Singapay balance by the
// sum of those residuals, which is a figure that can be queried and explained rather than a
// discrepancy that cannot.
func (a feeAdjustment) PlatformFeeRupiah() int64 {
	rupiah, _ := domain.MinorToRupiah(a.PlatformFeeMinor)
	return rupiah
}

// PlatformResidualMinor is the sub-rupiah part of the platform's settled fee, in sen: what
// the transfer moves but the rupiah-denominated ledger entry cannot express.
func (a feeAdjustment) PlatformResidualMinor() int64 {
	return a.PlatformFeeMinor - domain.RupiahToMinor(a.PlatformFeeRupiah())
}

// resolveFeeAdjustment applies the rules in docs/104-fee-mismatch-reconciliation.md.
//
// It returns a non-empty second value when the delta cannot be absorbed: Singapay took so
// much more than was quoted that the platform would have to pay in more than it ever
// charged. That is not a rounding to swallow — it means the quoted fee and the real one
// disagree by more than the transaction can carry, and no arithmetic makes the result
// correct. The transaction stays COMPLETED and keeps showing up as unsettled, which is the
// loud failure rather than the silent one.
//
// The block is the only place the fee model still matters. Under GATEWAY_ON_SELLER the
// platform fee is zero — that model is used for subscriptions, where the platform is itself
// the beneficiary and SkipPlatformFee is set — so any positive delta blocks rather than
// quietly taking the difference out of a seller who is the platform anyway. In practice the
// delta there is zero by construction: GATEWAY_ON_SELLER only ever runs over a payment link,
// and a payment link reports no fee, so the estimate is copied to the actual.
func resolveFeeAdjustment(tx *domain.ProductTransaction, settled domain.SettledTransaction) (feeAdjustment, string) {
	estimatedMinor := domain.RupiahToMinor(tx.Fee.GatewayFee)
	deltaMinor := settled.FeeMinor - estimatedMinor

	adj := feeAdjustment{
		FeeDeltaMinor:    deltaMinor,
		SellerNet:        tx.Fee.SellerNetAmount,
		PlatformFeeMinor: domain.RupiahToMinor(tx.Fee.PlatformFee) - deltaMinor,
	}

	if adj.PlatformFeeMinor < 0 {
		return adj, fmt.Sprintf(
			"the platform fee would be %s: the gateway overcharge (%s) exceeds the entire platform fee (%d)",
			formatMinor(adj.PlatformFeeMinor), formatMinor(deltaMinor), tx.Fee.PlatformFee)
	}

	return adj, ""
}

// formatMinor renders sen as rupiah with two decimals, for log lines and block reasons.
// A delta of 16 sen reported as "16" is the kind of thing that starts an investigation into
// a missing Rp16 that was never missing.
func formatMinor(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/domain.MinorPerRupiah, minor%domain.MinorPerRupiah)
}
