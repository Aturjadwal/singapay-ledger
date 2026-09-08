package ledger

import (
	"context"
	"time"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/ledgererr"
	"github.com/21strive/ledger/repo"
	"github.com/21strive/ledger/singapay"
)

// webhookReplayTolerance is how old a verified delivery may be before it is called stale.
//
// Singapay's own guidance is five minutes. What happens to a stale delivery is a separate
// decision, taken in warnIfStale: a forged webhook is refused, a late one is not.
const webhookReplayTolerance = 5 * time.Minute

// HandlePaymentSuccess books a money-in webhook: the payer paid, so the seller, the
// platform and the gateway expense account each get their PENDING entry.
//
// Pass the raw delivery. Verification comes first and the body is not read until it
// passes — a signature check on a payload you have already acted on is decoration.
//
// Three things about Singapay shape this:
//
// One callback URL carries four products. VA, QRIS, e-wallet and payment link all arrive
// on transaction_notif_url and are told apart by the envelope's event field — except that
// Singapay's own payment-link sample carries no event field at all, so the parser falls
// back to the payment method. Routing on event alone would silently drop payment-link
// confirmations.
//
// The merchant reference lives somewhere different on every channel, and the payment link
// is the trap: its transaction.reff_no is the id of one payment *attempt*, not the
// reference given at creation. MerchantReference resolves that; reading the fields
// directly would never match a payment link to its invoice.
//
// And Singapay retries. A duplicate delivery is ordinary traffic, not an incident, which
// is why a non-PENDING transaction is answered with success and no work.
func (c *LedgerClient) HandlePaymentSuccess(ctx context.Context, req singapay.WebhookRequest) error {
	if err := c.gateway.VerifyWebhook(req); err != nil {
		c.logger.WarnContext(ctx, "Rejected a money-in webhook that failed signature verification",
			"endpoint", req.Endpoint,
			"error", err,
		)
		return ledgererr.ErrWebhookVerificationFailed.WithError(err)
	}

	notification, err := singapay.ParseMoneyInNotification(req.Body)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "could not parse money-in notification", err)
	}

	c.warnIfStale(ctx, "money-in", req.Timestamp)

	invoiceNumber := notification.MerchantReference()
	if invoiceNumber == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"money-in notification carries no merchant reference; it cannot be matched to an invoice", nil)
	}

	if !notification.IsPaid() {
		// Not an error. An expired or failed instrument is a real event Singapay
		// reports on the same URL, and the transaction simply stays PENDING until it
		// expires on our side too.
		c.logger.InfoContext(ctx, "Money-in notification is not a payment",
			"invoice_number", invoiceNumber,
			"event", notification.Kind(),
			"status", notification.Data.Transaction.Status,
		)
		return nil
	}

	productTx, err := c.repoProvider.ProductTransaction().GetByInvoiceNumber(ctx, invoiceNumber)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return ledgererr.NewError(ledgererr.CodeNotFound, "product transaction not found", err)
		}
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get product transaction", err)
	}

	// Duplicate delivery, or a transaction already past PENDING. Singapay retries, so
	// this is expected traffic.
	if !productTx.IsPending() {
		c.logger.InfoContext(ctx, "Payment notification received for non-pending transaction",
			"invoice_number", invoiceNumber, "status", productTx.Status)
		return nil
	}

	// What the payer was actually charged, which is not always the transaction's own
	// amount field: QRIS adds a tip into total_amount, and e-wallet's documented sample
	// shows amount as the net against a gross total_amount.
	charged, chargedErr := notification.Charged().Rupiah()
	if chargedErr != nil {
		c.logger.WarnContext(ctx, "Money-in amount is not a whole rupiah value",
			"invoice_number", invoiceNumber,
			"amount", notification.Charged().String(),
			"error", chargedErr,
		)
	} else if charged != productTx.Fee.TotalCharged {
		// Booked anyway. The ledger's entries come from the transaction as priced, not
		// from the webhook, so a mismatch does not corrupt the books — but it means
		// somebody paid a different amount than we asked for, and settlement will
		// disagree. It belongs in the log and in the journal, loudly, now.
		c.logger.WarnContext(ctx, "Payer was charged a different amount than the transaction expects",
			"invoice_number", invoiceNumber,
			"expected", productTx.Fee.TotalCharged,
			"charged", charged,
			"difference", charged-productTx.Fee.TotalCharged,
		)
	}

	// The channel fee, where the channel reports one. Only a virtual account carries it
	// on the webhook; QRIS and e-wallet expose theirs on the transaction record, and a
	// payment link nowhere at all. Recording it here turns a fee mismatch into something
	// visible at payment time rather than a surprise at settlement.
	var reportedFee int64
	if fee, ok := notification.ChannelFee(); ok {
		if rupiah, err := fee.Rupiah(); err == nil {
			reportedFee = rupiah
			if reportedFee != productTx.Fee.GatewayFee {
				c.logger.WarnContext(ctx, "Channel fee on the webhook differs from the fee expected at payment time — settlement will carry an adjustment",
					"invoice_number", invoiceNumber,
					"expected_gateway_fee", productTx.Fee.GatewayFee,
					"reported_gateway_fee", reportedFee,
					"fee_delta", reportedFee-productTx.Fee.GatewayFee,
				)
			}
		}
	}

	paymentReq, err := c.repoProvider.PaymentRequest().GetByProductTransactionID(ctx, productTx.UUID)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get payment request", err)
	}

	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get platform account", err)
	}

	gatewayAccount, err := c.repoProvider.Account().GetPaymentGatewayAccount(ctx)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get payment gateway account", err)
	}

	journal := domain.NewJournal(
		domain.EventTypePaymentSuccess,
		domain.SourceTypeProductTransaction,
		productTx.UUID,
		map[string]any{
			"invoice_number":       invoiceNumber,
			"event":                string(notification.Kind()),
			"gateway_transaction":  notification.Data.Transaction.TransactionID,
			"seller_price":         productTx.Fee.SellerPrice,
			"seller_net_amount":    productTx.Fee.SellerNetAmount,
			"platform_fee":         productTx.Fee.PlatformFee,
			"gateway_fee":          productTx.Fee.GatewayFee,
			"reported_gateway_fee": reportedFee,
			"charged":              charged,
			"fee_model":            productTx.Fee.FeeModel,
		},
	)

	// PENDING credits for seller, platform and the gateway expense account. The amounts
	// come from the transaction as priced — the fee Singapay actually took is reconciled
	// at settlement, not here, because until then it is not final.
	ledgerEntries := domain.NewPaymentEntries(
		journal.UUID,
		productTx.UUID,
		productTx.SellerAccountID,
		productTx.Fee.SellerNetAmount,
		platformAccount.UUID,
		productTx.Fee.PlatformFee,
		gatewayAccount.UUID,
		productTx.Fee.GatewayFee,
	)

	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.Journal().Save(ctx, journal); err != nil {
			return err
		}

		if err := paymentReq.MarkCompleted(); err != nil {
			return err
		}
		if err := tx.PaymentRequest().Update(ctx, paymentReq); err != nil {
			return err
		}

		if err := productTx.MarkCompleted(); err != nil {
			return err
		}
		if err := tx.ProductTransaction().UpdateStatus(ctx, productTx.UUID, productTx.Status, *productTx.CompletedAt); err != nil {
			return err
		}

		return tx.LedgerEntry().SaveBatch(ctx, ledgerEntries)
	})

	if err != nil {
		c.logger.ErrorContext(ctx, "Failed to persist payment success", "invoice_number", invoiceNumber, "error", err)
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to persist payment success transaction", err)
	}

	c.logger.InfoContext(ctx, "Payment success booked",
		"invoice_number", invoiceNumber,
		"product_tx_id", productTx.UUID,
		"event", notification.Kind(),
	)

	return nil
}

// HandleDisbursementNotification books the outcome of a payout Singapay has finished
// deciding on.
//
// This handler is not optional the way a money-in one might be. A Singapay payout is
// asynchronous: the transfer call answers SP000 to say the instruction was accepted, and
// the money may still be hours from moving or may never move. Without this webhook a
// seller's balance stays reserved against a payout that failed, until somebody runs the
// pending sweep by hand.
//
// The payload's Data is the same shape the transfer and inquiry endpoints return, so it is
// booked through the same code path as those — one set of rules for all three.
func (c *LedgerClient) HandleDisbursementNotification(ctx context.Context, req singapay.WebhookRequest) error {
	if err := c.gateway.VerifyWebhook(req); err != nil {
		c.logger.WarnContext(ctx, "Rejected a money-out webhook that failed signature verification",
			"endpoint", req.Endpoint,
			"error", err,
		)
		return ledgererr.ErrWebhookVerificationFailed.WithError(err)
	}

	notification, err := singapay.ParseMoneyOutNotification(req.Body)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "could not parse money-out notification", err)
	}

	c.warnIfStale(ctx, "money-out", req.Timestamp)

	// disbursement_notif_url carries three products. Only bank disbursement is ours;
	// e-wallet top-up and QRIS issuer are not operations this ledger performs.
	if notification.Event != "" && notification.Event != singapay.EventDisbursement {
		c.logger.InfoContext(ctx, "Ignoring a money-out notification for another product",
			"event", notification.Event)
		return nil
	}

	reference := notification.Data.ReferenceNumber
	if reference == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"money-out notification carries no reference number; it cannot be matched to a disbursement", nil)
	}

	disbursement, err := c.repoProvider.Disbursement().GetByPayoutRequestID(ctx, reference)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			// Worth a warning rather than silence: a payout Singapay is telling us about
			// that we have no row for means either a reference we never stored or a
			// delivery meant for a different environment.
			c.logger.WarnContext(ctx, "Money-out notification references a payout this ledger has no row for",
				"reference_number", reference,
				"transaction_id", notification.Data.TransactionID,
			)
			return ledgererr.ErrDisbursementNotFound.WithError(err)
		}
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to load disbursement", err)
	}

	// Terminal rows are left alone. A retried delivery for a payout already booked
	// COMPLETED or FAILED must not write a second reversal.
	if !disbursement.IsPending() && !disbursement.IsProcessing() {
		c.logger.InfoContext(ctx, "Money-out notification for an already terminal disbursement — nothing to do",
			"disbursement_id", disbursement.UUID,
			"status", disbursement.Status,
		)
		return nil
	}

	account, err := c.repoProvider.Account().GetByID(ctx, disbursement.LedgerUUID)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get account for disbursement", err)
	}

	data := notification.Data
	if _, err := c.bookPayoutOutcome(ctx, account, disbursement, &data); err != nil {
		return err
	}
	return nil
}

// warnIfStale logs a verified delivery that arrived outside the replay window, and
// deliberately does not refuse it.
//
// A forged webhook is refused by the signature check. This one is genuine — Singapay
// signed it — and merely late, which during an outage or a redelivery storm is the normal
// case. Dropping it would leave a paid transaction unbooked or a payout stuck in flight,
// and the ledger already refuses to double-book: a money-in webhook is ignored unless the
// transaction is PENDING, and a payout outcome is ignored unless the row is still open. So
// the stale delivery is processed and the fact is recorded.
func (c *LedgerClient) warnIfStale(ctx context.Context, kind, timestamp string) {
	if timestamp == "" {
		return
	}
	if singapay.WebhookFresh(timestamp, time.Now(), webhookReplayTolerance) {
		return
	}
	c.logger.WarnContext(ctx, "Verified webhook arrived outside the replay window — processing it anyway",
		"kind", kind,
		"timestamp", timestamp,
		"tolerance", webhookReplayTolerance.String(),
	)
}
