package ledger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
// shrinking rather than by the size of the merchant. (A payment link with no identifier
// stored costs a search of its account's history the first time; see
// readPaymentLinkHistory.)
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
		// A transaction another pass settled first is settled all the same; the pass
		// counts what is settled, not which caller wrote it.
		case settleOutcomeSettled, settleOutcomeAlreadySettled:
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
	// settleOutcomeAlreadySettled is bookSettlement losing the race to another caller: the
	// transaction is settled, and this call wrote nothing. The pass counts it as settled;
	// CheckTransactionSettlement reports it apart, because "this check settled it" and
	// "it was already settled" are different answers to the person who pressed the button.
	settleOutcomeAlreadySettled
)

// settleOne asks Singapay about one open invoice and, if the funds have settled, books it.
//
// The read is about this one transaction: the channel says which endpoint, and the payment
// request says which key — or, for a payment link with none stored yet, what to look for in
// the account's history. Nothing about a settlement batch enters into it, which is what
// makes the result independent of how the batch's window is interpreted.
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
// It is readGatewayTransaction with everything short of a settlement folded into nil:
// the settling pass has no use for a payment that has not settled, only for one that has.
func (c *LedgerClient) readSettledTransaction(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
) (*domain.SettledTransaction, error) {
	reading, err := c.readGatewayTransaction(ctx, gatewayAccountID, tx, paymentReq)
	if err != nil {
		return nil, err
	}
	if !reading.Found || !reading.HasSettle {
		return nil, nil
	}
	settled := reading.Settled
	return &settled, nil
}

// gatewayReading is what one point lookup at Singapay said about a transaction, whether
// or not its funds have settled.
//
// Settled carries the figures exactly as a settlement would book them. They are only
// booked when HasSettle is true: before that, a QRIS row has no settled-to-merchant amount
// yet, so its net and fee are not figures to act on.
type gatewayReading struct {
	// Found is false when Singapay holds no payment against the instrument: a virtual
	// account nobody has paid into, a payment link with no attempt.
	Found bool
	// HasSettle is Singapay's has_settle: the funds have reached the merchant balance.
	HasSettle bool
	// Status is Singapay's own status for the payment, as it wrote it.
	Status string
	// SettleAt is Singapay's settle_at, nil when it reported none.
	SettleAt *time.Time
	Settled  domain.SettledTransaction
}

// readGatewayTransaction reads one transaction back from Singapay.
//
// Key selection is the interesting part. GatewayTransactionID and GatewayTransactionRef
// come from the money-in webhook and are the direct keys; RequestID and PaymentCode, from
// instrument creation, are the fallbacks for rows that predate those columns. The fallback
// differs per channel because the instrument and the transaction are the same entity for
// QRIS and e-wallet and different entities for VA and payment link (a card payment
// included, since it is one). A payment link has no creation-time key for its payment at
// all, so without a stored one the attempt is searched for — readPaymentLinkHistory — and
// what the search finds is stored, so it is only searched for once.
func (c *LedgerClient) readGatewayTransaction(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
) (*gatewayReading, error) {
	kind := paymentChannelKind(paymentReq.PaymentChannel)
	switch kind {

	case channelVirtualAccount:
		va, err := c.readVATransaction(ctx, gatewayAccountID, paymentReq)
		if err != nil {
			return nil, err
		}
		if va == nil {
			return &gatewayReading{}, nil
		}
		// The VA fee is a single reported figure, so net is derived rather than read.
		gross := va.Amount.Minor()
		fee := va.Fees.Amount.Minor()
		return &gatewayReading{
			Found:     true,
			HasSettle: va.HasSettle,
			Status:    string(va.Status),
			SettleAt:  millisTimePtr(va.SettleAt),
			Settled: domain.SettledTransaction{
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
		// Derive the fee from the net rather than by summing MDRCost, VendorFee and
		// OurMargin. How those three relate to the net is not documented, and Singapay's
		// own field examples do not add up to it — 150 + 50 against a 250 difference. The
		// net is the figure that decides what the seller actually received, so it is the
		// one to trust until the decomposition is confirmed against a sandbox.
		gross := qr.TotalAmount.Minor()
		net := qr.SettledToMerchant.Minor()
		return &gatewayReading{
			Found:     true,
			HasSettle: qr.HasSettle,
			Status:    string(qr.Status),
			SettleAt:  isoTimePtr(qr.SettleAt),
			Settled: domain.SettledTransaction{
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
			},
		}, nil

	case channelEwallet:
		key := storedGatewayTransactionID(paymentReq)
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
		return &gatewayReading{
			Found:     true,
			HasSettle: ew.HasSettle,
			Status:    string(ew.Status),
			SettleAt:  isoTimePtr(ew.SettleAt),
			Settled: domain.SettledTransaction{
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
			},
		}, nil

	// A card payment is a payment link pinned to the card methods, so it settles as one:
	// same history row, same absent fee. Only the channel it is booked under differs.
	case channelPaymentLink, channelCard:
		history, route, err := c.readPaymentLinkHistory(ctx, gatewayAccountID, tx, paymentReq)
		if err != nil {
			return nil, err
		}
		if history == nil {
			return &gatewayReading{}, nil
		}
		// Remembered whether or not it has settled yet: a card settles days after it is
		// paid, and the pass would otherwise search for the same attempt every day until then.
		if route != paymentLinkByStoredID && tookPayment(history) {
			c.rememberPaymentLinkAttempt(ctx, tx, paymentReq, history, route)
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
		channel := ChannelPaymentLink
		if kind == channelCard {
			channel = ChannelCreditCard
		}
		return &gatewayReading{
			Found:     true,
			HasSettle: history.HasSettle,
			Status:    string(history.Status),
			SettleAt:  isoTimePtr(history.SettleAt),
			Settled: domain.SettledTransaction{
				MerchantReference:    tx.InvoiceNumber,
				GatewayTransactionID: strconv.FormatInt(history.ID, 10),
				GatewayAccountID:     gatewayAccountID,
				PaymentChannel:       channel,
				GrossMinor:           gross,
				NetMinor:             gross - feeMinor,
				FeeMinor:             feeMinor,
				FeeReported:          false,
				Raw: map[string]string{
					"reff_no":              history.ReffNo,
					"payment_link_reff_no": history.PaymentLinkReffNo,
					"status":               string(history.Status),
				},
			},
		}, nil

	default:
		return nil, fmt.Errorf("payment channel %q is not a Singapay money-in product", paymentReq.PaymentChannel)
	}
}

// millisTimePtr and isoTimePtr turn Singapay's optional timestamps into nil when absent,
// so "not reported" never reads as the zero time.
func millisTimePtr(t singapay.MillisTime) *time.Time {
	if !t.Set {
		return nil
	}
	v := t.Time
	return &v
}

func isoTimePtr(t singapay.ISOTime) *time.Time {
	if !t.Set {
		return nil
	}
	v := t.Time
	return &v
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

// The bounds of the payment-link history scan in scanPaymentLinkHistories.
const (
	// paymentLinkScanPerPage is the page size asked for. Singapay may answer with fewer,
	// which costs pages but never rows: the scan walks page numbers, not offsets.
	paymentLinkScanPerPage = 100
	// paymentLinkScanMaxPages caps one scan at 2,000 attempts on the seller's sub-account
	// since the transaction was created. A scan that reaches it without an answer is an
	// error, never a "not found".
	paymentLinkScanMaxPages = 20
	// paymentLinkScanMargin is the clock tolerance between Singapay and the database when
	// deciding the history has gone back past the transaction.
	paymentLinkScanMargin = time.Hour
)

// paymentLinkRoute names how readPaymentLinkHistory reached an attempt: for the log line,
// and for knowing whether the attempt's identifiers are already stored.
type paymentLinkRoute string

const (
	paymentLinkByStoredID  paymentLinkRoute = "stored_id"
	paymentLinkByStoredRef paymentLinkRoute = "stored_reference"
	paymentLinkByScan      paymentLinkRoute = "scan"
)

// readPaymentLinkHistory finds the attempt that paid a transaction's payment link.
//
// Singapay reads a payment-link payment back by payment_link_histories.id, the id of one
// ATTEMPT. Creation returns the LINK's id, which that endpoint does not accept, and a
// payment-link webhook carries no numeric id — so the attempt is reached by one of three
// routes, cheapest first:
//
//  1. A stored attempt id: a point lookup. The row is checked against the invoice before
//     anything is read from it, and a mismatch is an error — an id that points at another
//     payment would book that payment's amount here.
//  2. A stored attempt reference, which the payment-link webhook reports as
//     transaction.reff_no: the history listing filtered on reff_no, matched exactly. A miss
//     proves nothing — that the webhook's reference is the history row's reff_no has not
//     been confirmed against Singapay — so it falls through to the scan.
//  3. The scan: the account's whole history, newest first, matched on
//     payment_link_reff_no, the reference payment creation set to the invoice number. See
//     scanPaymentLinkHistories.
//
// The listing is NOT filtered on the invoice number. v0.7.0 did that, and it never matched:
// Singapay's reff_no filter does not select on payment_link_reff_no — production showed
// that — and by every sign selects on a row's own reff_no, the attempt's reference, which
// never holds the invoice. Its empty answer was then read as "Singapay holds no payment",
// and no card or payment-link transaction ever settled.
//
// A nil attempt with no error means the scan proved there is none. Anything short of proof
// is an error.
func (c *LedgerClient) readPaymentLinkHistory(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
) (*singapay.PaymentLinkHistory, paymentLinkRoute, error) {
	ref := strings.TrimSpace(paymentReq.GatewayTransactionRef)

	if stored := storedGatewayTransactionID(paymentReq); stored != "" {
		if id, err := strconv.ParseInt(stored, 10, 64); err == nil {
			history, err := c.gateway.GetPaymentLinkHistory(ctx, gatewayAccountID, id)
			if err != nil {
				return nil, "", fmt.Errorf("failed to read the payment link history: %w", err)
			}
			if !historyBelongsTo(history, tx.InvoiceNumber, ref) {
				return nil, "", fmt.Errorf("payment link history %d belongs to %q, not %q: refusing to read this transaction's payment from it",
					id, historyOwner(history), tx.InvoiceNumber)
			}
			return history, paymentLinkByStoredID, nil
		}
	}

	if ref != "" {
		rows, _, err := c.gateway.ListPaymentLinkHistories(ctx, gatewayAccountID, singapay.SettlementWindow{
			ReffNo:  ref,
			PerPage: 25,
		})
		if err != nil {
			return nil, "", fmt.Errorf("failed to list payment link histories by attempt reference %q: %w", ref, err)
		}
		// The filter matches partially, so the comparison is made exactly here.
		for i := range rows {
			if rows[i].ReffNo == ref && historyBelongsTo(&rows[i], tx.InvoiceNumber, ref) {
				return &rows[i], paymentLinkByStoredRef, nil
			}
		}
	}

	history, err := c.scanPaymentLinkHistories(ctx, gatewayAccountID, tx, ref)
	if err != nil {
		return nil, "", err
	}
	return history, paymentLinkByScan, nil
}

// scanPaymentLinkHistories walks a sub-account's payment-link history, newest first, for
// the attempt that paid a transaction's link.
//
// A link can carry several attempts — a QRIS code generated and abandoned, then a VA that
// was paid — and only one of them took the money. An attempt that did (tookPayment) ends
// the scan; one that did not is kept, and is the answer only if the history runs out
// without a paid one.
//
// "Not found" is an answer, so it has to be proven: the pass reads it as "not settled yet"
// and an operator's check shows it as such. The scan concludes only on evidence that
// nothing further down could be this transaction's:
//
//   - a page with no rows: the history is exhausted;
//   - the last page by Singapay's own total_pages;
//   - a page whose dated rows were all created before the transaction, less
//     paymentLinkScanMargin. The list is newest first and an attempt cannot predate the
//     link, which is created with the transaction, so everything after such a page is
//     older still. A row without a created_at is no evidence either way, and a page with
//     none dated proves nothing.
//
// A short page is not on that list: Singapay may cap per_page, so fewer rows than asked for
// does not make it the last page. Running out of pages without one of the above is an
// error, as is any failed call — the context's deadline included, which the on-demand
// check sets.
func (c *LedgerClient) scanPaymentLinkHistories(
	ctx context.Context,
	gatewayAccountID string,
	tx *domain.ProductTransaction,
	ref string,
) (*singapay.PaymentLinkHistory, error) {
	cutoff := tx.CreatedAt.Add(-paymentLinkScanMargin)
	var unpaid *singapay.PaymentLinkHistory

	for page := 1; page <= paymentLinkScanMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("stopped scanning the payment link history for %s before page %d: %w", tx.InvoiceNumber, page, err)
		}
		rows, pagination, err := c.gateway.ListPaymentLinkHistories(ctx, gatewayAccountID, singapay.SettlementWindow{
			Page:    page,
			PerPage: paymentLinkScanPerPage,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to scan the payment link history for %s at page %d: %w", tx.InvoiceNumber, page, err)
		}

		for i := range rows {
			if !historyBelongsTo(&rows[i], tx.InvoiceNumber, ref) {
				continue
			}
			if tookPayment(&rows[i]) {
				return &rows[i], nil
			}
			if unpaid == nil {
				unpaid = &rows[i]
			}
		}

		lastPage := pagination.TotalPages > 0 && page >= pagination.TotalPages
		if len(rows) == 0 || lastPage || datedRowsAllBefore(rows, cutoff) {
			return unpaid, nil
		}
	}

	return nil, fmt.Errorf(
		"the payment link history scan for %s was inconclusive: %d pages of up to %d attempts did not reach back to the transaction's creation (%s), and none of them paid it",
		tx.InvoiceNumber, paymentLinkScanMaxPages, paymentLinkScanPerPage, tx.CreatedAt.UTC().Format(time.RFC3339))
}

// historyBelongsTo reports whether a payment-link history row is an attempt at this
// transaction's link.
//
// payment_link_reff_no is the link's reference, which payment creation sets to the invoice
// number. reff_no is the attempt's own reference, not the invoice; it is still compared, as
// v0.7.0 compared it, because an invoice found there would be no less this transaction's.
// A row that matches nothing but the stored attempt reference is taken only when it names
// no link reference at all: one that names another invoice is that invoice's, whatever its
// reff_no says.
func historyBelongsTo(h *singapay.PaymentLinkHistory, invoiceNumber, ref string) bool {
	if invoiceNumber != "" && (h.PaymentLinkReffNo == invoiceNumber || h.ReffNo == invoiceNumber) {
		return true
	}
	return ref != "" && h.ReffNo == ref && h.PaymentLinkReffNo == ""
}

// historyOwner is the reference a history row says it belongs to, for an error message.
func historyOwner(h *singapay.PaymentLinkHistory) string {
	if h.PaymentLinkReffNo != "" {
		return h.PaymentLinkReffNo
	}
	return h.ReffNo
}

// tookPayment reports whether an attempt is the one that took the money: Singapay marked it
// paid, or has already settled it.
func tookPayment(h *singapay.PaymentLinkHistory) bool {
	return h.Status == singapay.PaymentPaid || h.HasSettle
}

// datedRowsAllBefore reports whether a page proves the history has gone back past t: it
// holds at least one row with a created_at, and every such row was created before t.
func datedRowsAllBefore(rows []singapay.PaymentLinkHistory, t time.Time) bool {
	dated := false
	for i := range rows {
		if !rows[i].CreatedAt.Set {
			continue
		}
		if !rows[i].CreatedAt.Time.Before(t) {
			return false
		}
		dated = true
	}
	return dated
}

// rememberPaymentLinkAttempt stores the identifiers of an attempt a search found, so the
// next read of the transaction is the point lookup rather than another search.
//
// Only the attempt that took the payment is passed here: an abandoned attempt's id, once
// stored, would be read back as the payment from then on. The write is
// RecordGatewayTransaction's conditional one, which fills what is missing and never
// replaces a valid identifier. And it is best-effort — the read has already succeeded, and
// a settlement is not failed because a shortcut could not be written; the next read simply
// searches again.
func (c *LedgerClient) rememberPaymentLinkAttempt(
	ctx context.Context,
	tx *domain.ProductTransaction,
	paymentReq *domain.PaymentRequest,
	history *singapay.PaymentLinkHistory,
	route paymentLinkRoute,
) {
	var id string
	if history.ID > 0 {
		id = strconv.FormatInt(history.ID, 10)
	}
	ref := strings.TrimSpace(history.ReffNo)
	if id == "" && ref == "" {
		return
	}

	recorded, err := c.repoProvider.PaymentRequest().RecordGatewayTransaction(ctx, paymentReq.UUID, id, ref)
	if err != nil {
		c.logger.WarnContext(ctx, "Could not store the payment link attempt a search found; the next read searches again",
			"product_transaction_uuid", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
			"payment_link_history_id", id,
			"found_by", string(route),
			"error", err,
		)
		return
	}
	if recorded {
		c.logger.InfoContext(ctx, "Stored the payment link attempt a search found",
			"product_transaction_uuid", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
			"payment_link_history_id", id,
			"payment_link_attempt_reference", ref,
			"found_by", string(route),
		)
	}
}

// gatewayNumericID resolves the numeric transaction id for the channels whose detail
// endpoint takes one, preferring the webhook-sourced value over the creation-time one.
func gatewayNumericID(paymentReq *domain.PaymentRequest) (int64, error) {
	raw := storedGatewayTransactionID(paymentReq)
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

// storedGatewayTransactionID is the stored payment id, or "" when there is none. A "0"
// counts as none: the money-in webhook stored its numeric id as given, and a delivery
// without one — a card paid through a payment link is the case seen in production — left
// "0", which no Singapay endpoint resolves. Read as a real id it sent every lookup to
// history_id 0 and a 404, so the transaction never settled; read as absent, the lookup
// takes its fallback like any row that predates the column.
func storedGatewayTransactionID(paymentReq *domain.PaymentRequest) string {
	raw := strings.TrimSpace(paymentReq.GatewayTransactionID)
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id <= 0 {
		return ""
	}
	return raw
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
		// Another pass, or an on-demand check, got there first. Nothing was written here,
		// and the caller's question — is this settled? — is answered yes.
		c.logger.InfoContext(ctx, "Transaction was already settled by another pass",
			"product_transaction_uuid", tx.UUID,
			"invoice_number", tx.InvoiceNumber,
		)
		return settleOutcomeAlreadySettled, nil
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
