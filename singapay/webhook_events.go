package singapay

import (
	"encoding/json"
	"fmt"
)

// MoneyInEvent identifies which product a money-in webhook came from. All four share one
// callback URL (transaction_notif_url) and are told apart by the envelope's event field.
type MoneyInEvent string

const (
	EventVATransaction MoneyInEvent = "va-transaction"
	EventQRISAcquirer  MoneyInEvent = "qris-acquirer-transaction"
	EventPaymentLink   MoneyInEvent = "payment-link-transaction"
	EventEwalletNative MoneyInEvent = "ewallet-native-transaction"
)

// PaymentStatus is the state of a money-in transaction.
type PaymentStatus string

const (
	PaymentPaid    PaymentStatus = "paid"
	PaymentUnpaid  PaymentStatus = "unpaid"
	PaymentPending PaymentStatus = "pending"
	PaymentExpired PaymentStatus = "expired"
	PaymentFailed  PaymentStatus = "failed"
)

// Customer is the payer, when the channel captured any of it.
type Customer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Phone string `json:"phone"`
}

// MoneyInTransaction is the transaction block shared by all four money-in webhooks.
//
// The fields are shared; their meaning is not. On QRIS, TotalAmount is Amount plus Tip.
// On e-wallet, the documented sample shows Amount 95000 against TotalAmount 100000 — net
// versus gross. Do not treat the pair as interchangeable across channels; see
// [MoneyInNotification.Charged].
type MoneyInTransaction struct {
	ID     int64  `json:"id"`
	ReffNo string `json:"reff_no"`
	// MerchantReffNo is present on QRIS and e-wallet. It is absent on VA — where
	// ReffNo carries the merchant reference instead — and on payment link, where the
	// merchant reference lives on the payment link itself. Use
	// [MoneyInNotification.MerchantReference] rather than reading these directly.
	MerchantReffNo string `json:"merchant_reff_no"`
	TransactionID  string `json:"transaction_id"`

	Type          string        `json:"type"` // va, qris, pl, ewallet
	Status        PaymentStatus `json:"status"`
	EwalletVendor string        `json:"ewallet_vendor"`
	AmountType    string        `json:"amount_type"`

	Amount      Amount `json:"amount"`
	TotalAmount Amount `json:"total_amount"`
	Tip         Amount `json:"tip"`

	PostedAt    TextTime `json:"post_timestamp"`
	ProcessedAt TextTime `json:"processed_timestamp"`
}

// MoneyInPayment describes the channel used. AdditionalInfo is channel-specific and is
// read through the typed accessors on [MoneyInNotification].
type MoneyInPayment struct {
	Method         string          `json:"method"`
	Vendor         string          `json:"vendor"`
	AdditionalInfo json.RawMessage `json:"additional_info"`
}

// MoneyInNotification is a payment confirmation delivered to transaction_notif_url.
type MoneyInNotification struct {
	Status  int          `json:"status"`
	Success bool         `json:"success"`
	Event   MoneyInEvent `json:"event"`
	// Timestamp is when the event fired. Money-in webhooks write times as text
	// ("26 Dec 2025 13:35:45"), unlike money-out which uses epoch milliseconds.
	Timestamp TextTime `json:"timestamp"`

	Data struct {
		Transaction MoneyInTransaction `json:"transaction"`
		Customer    Customer           `json:"customer"`
		Payment     MoneyInPayment     `json:"payment"`
	} `json:"data"`
}

// ParseMoneyInNotification decodes a verified money-in webhook body.
//
// Verify the signature first with [Client.VerifyWebhook]: this only parses.
func ParseMoneyInNotification(body []byte) (*MoneyInNotification, error) {
	var n MoneyInNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("singapay: cannot parse money-in webhook: %w", err)
	}
	if n.Kind() == "" {
		return nil, fmt.Errorf("singapay: webhook has neither a recognised event nor payment method")
	}
	return &n, nil
}

// Kind resolves which product this webhook came from.
//
// It prefers the envelope's event field, but falls back to the payment method: Singapay's
// documented payment-link sample carries no event field at all, while the VA, QRIS and
// e-wallet samples do. Routing on event alone would drop payment-link confirmations.
func (n *MoneyInNotification) Kind() MoneyInEvent {
	switch n.Event {
	case EventVATransaction, EventQRISAcquirer, EventPaymentLink, EventEwalletNative:
		return n.Event
	}
	switch n.Data.Payment.Method {
	case "va":
		return EventVATransaction
	case "qris":
		return EventQRISAcquirer
	case "payment_link":
		return EventPaymentLink
	case "ewallet":
		return EventEwalletNative
	}
	return ""
}

// IsPaid reports whether money actually arrived.
func (n *MoneyInNotification) IsPaid() bool { return n.Data.Transaction.Status == PaymentPaid }

// MerchantReference returns the reference this system supplied when the payment
// instrument was created — the key that matches a transaction back to an invoice.
//
// Every channel puts it somewhere different, and the payment link is the trap: its
// transaction.reff_no is the id of one payment *attempt*, not the reference given at
// creation, which is nested on the payment_link object instead. Matching an invoice on
// transaction.reff_no would silently never match for payment links.
func (n *MoneyInNotification) MerchantReference() string {
	if ref := n.Data.Transaction.MerchantReffNo; ref != "" {
		return ref
	}
	if n.Kind() == EventPaymentLink {
		if info, err := n.PaymentLinkInfo(); err == nil && info.ReffNo != "" {
			return info.ReffNo
		}
		return ""
	}
	// VA carries it directly in reff_no.
	return n.Data.Transaction.ReffNo
}

// Charged returns the amount the customer actually paid.
//
// TotalAmount is preferred where present — on QRIS it includes the tip, and on e-wallet
// the documented sample shows it as the gross against a net Amount. Where Singapay sends
// only Amount, that is the charge.
func (n *MoneyInNotification) Charged() Amount {
	if n.Data.Transaction.TotalAmount.Set {
		return n.Data.Transaction.TotalAmount
	}
	return n.Data.Transaction.Amount
}

// ChannelFee returns the fee Singapay deducted for this transaction, when the channel
// reports one.
//
// Only Virtual Account carries it in the webhook. QRIS and e-wallet expose their fees on
// the transaction record rather than the callback, and payment link does not expose a
// per-transaction fee anywhere at all — which is why a fee-mismatch reconciliation cannot
// be built on payment links.
func (n *MoneyInNotification) ChannelFee() (Amount, bool) {
	if n.Kind() != EventVATransaction {
		return Amount{}, false
	}
	info, err := n.VAInfo()
	if err != nil || !info.Fees.Amount.Set {
		return Amount{}, false
	}
	return info.Fees.Amount, true
}

// ChannelBank identifies the issuing bank of a VA payment.
type ChannelBank struct {
	ShortName string `json:"short_name"`
	Number    string `json:"number"`
	SwiftCode string `json:"swift_code"`
	BankCode  string `json:"bank_code"`
}

// VAAdditionalInfo is the channel detail on a virtual-account webhook.
type VAAdditionalInfo struct {
	VANumber string      `json:"va_number"`
	VAName   string      `json:"va_name"`
	Bank     ChannelBank `json:"bank"`
	Fees     struct {
		Name     string `json:"name"`
		Amount   Amount `json:"amount"`
		Currency string `json:"currency"`
	} `json:"fees"`
}

// VAInfo decodes the VA-specific detail.
func (n *MoneyInNotification) VAInfo() (*VAAdditionalInfo, error) {
	return decodeInfo[VAAdditionalInfo](n, EventVATransaction)
}

// QRISAdditionalInfo is the channel detail on a QRIS webhook.
type QRISAdditionalInfo struct {
	QRString       string `json:"qr_string"`
	PaymentEventID int64  `json:"payment_event_id"`
}

// QRISInfo decodes the QRIS-specific detail.
func (n *MoneyInNotification) QRISInfo() (*QRISAdditionalInfo, error) {
	return decodeInfo[QRISAdditionalInfo](n, EventQRISAcquirer)
}

// EwalletAdditionalInfo is the channel detail on an e-wallet webhook.
type EwalletAdditionalInfo struct {
	PaymentEventID    int64  `json:"payment_event_id"`
	VendorReferenceNo string `json:"vendor_reference_no"`
}

// EwalletInfo decodes the e-wallet-specific detail.
func (n *MoneyInNotification) EwalletInfo() (*EwalletAdditionalInfo, error) {
	return decodeInfo[EwalletAdditionalInfo](n, EventEwalletNative)
}

// PaymentLinkInfo is the parent payment link carried on a payment-link webhook. Its
// ReffNo is the merchant reference; the transaction's own reff_no is the attempt id.
type PaymentLinkInfo struct {
	ID           int64   `json:"id"`
	ReffNo       string  `json:"reff_no"`
	Title        string  `json:"title"`
	PaymentURL   string  `json:"payment_url"`
	Status       string  `json:"status"`
	MaxUsage     int     `json:"max_usage"`
	CurrentUsage int     `json:"current_usage"`
	TotalAmount  Amount  `json:"total_amount"`
	AccountID    int64   `json:"account_id"`
	PaymentDate  ISOTime `json:"payment_date"`
}

// PaymentLinkInfo decodes the payment-link-specific detail.
func (n *MoneyInNotification) PaymentLinkInfo() (*PaymentLinkInfo, error) {
	var wrapper struct {
		PaymentLink PaymentLinkInfo `json:"payment_link"`
	}
	if n.Kind() != EventPaymentLink {
		return nil, fmt.Errorf("singapay: not a payment-link webhook (%s)", n.Kind())
	}
	if len(n.Data.Payment.AdditionalInfo) == 0 {
		return nil, fmt.Errorf("singapay: webhook carries no additional_info")
	}
	if err := json.Unmarshal(n.Data.Payment.AdditionalInfo, &wrapper); err != nil {
		return nil, fmt.Errorf("singapay: cannot decode payment_link detail: %w", err)
	}
	return &wrapper.PaymentLink, nil
}

func decodeInfo[T any](n *MoneyInNotification, want MoneyInEvent) (*T, error) {
	if n.Kind() != want {
		return nil, fmt.Errorf("singapay: webhook is %s, not %s", n.Kind(), want)
	}
	if len(n.Data.Payment.AdditionalInfo) == 0 {
		return nil, fmt.Errorf("singapay: webhook carries no additional_info")
	}
	var out T
	if err := json.Unmarshal(n.Data.Payment.AdditionalInfo, &out); err != nil {
		return nil, fmt.Errorf("singapay: cannot decode %s detail: %w", want, err)
	}
	return &out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Money out
// ─────────────────────────────────────────────────────────────────────────────

// MoneyOutEvent identifies which money-out product a disbursement_notif_url callback came
// from. All three share that URL.
type MoneyOutEvent string

const (
	EventDisbursement MoneyOutEvent = "disbursement"
	EventEwalletTopUp MoneyOutEvent = "ewallet-topup"
	EventQRISIssuer   MoneyOutEvent = "qris-issuer"
)

// MoneyOutNotification is a payout outcome delivered to disbursement_notif_url.
//
// Its Data is the same shape the transfer and inquiry endpoints return, so a webhook and
// a poll can be booked by one code path. Note ResponseCode here reports the *transaction*
// result rather than a request result — a delivery can arrive carrying SP001 to say a
// previously accepted transfer has since failed.
type MoneyOutNotification struct {
	ResponseCode    ResponseCode  `json:"response_code"`
	ResponseMessage string        `json:"response_message"`
	Event           MoneyOutEvent `json:"event"`
	Data            Disbursement  `json:"data"`
}

// ParseMoneyOutNotification decodes a verified money-out webhook body.
func ParseMoneyOutNotification(body []byte) (*MoneyOutNotification, error) {
	var n MoneyOutNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("singapay: cannot parse money-out webhook: %w", err)
	}
	return &n, nil
}

// Succeeded reports whether the payout completed.
func (n *MoneyOutNotification) Succeeded() bool { return n.Data.TransactionStatus().Succeeded() }

// ─────────────────────────────────────────────────────────────────────────────
// Settlement
// ─────────────────────────────────────────────────────────────────────────────

// SettlementEvent is the lifecycle event on settlement_notif_url.
type SettlementEvent string

const (
	EventSettlementCompleted       SettlementEvent = "settlement.completed"
	EventSettlementRefunded        SettlementEvent = "settlement.refunded"
	EventSettlementRefundCancelled SettlementEvent = "settlement.refund_cancelled"
)

// SettlementMethod is how a batch moved the money.
type SettlementMethod string

const (
	// SettlementToBalance moves pending balance into available balance — the case
	// that matters to a ledger tracking PENDING and AVAILABLE.
	SettlementToBalance SettlementMethod = "balance"
	// SettlementAutoBalance is the same, created by the scheduler rather than approved.
	SettlementAutoBalance SettlementMethod = "auto-balance"
	// SettlementToBank sends funds out to a nominated bank account instead.
	SettlementToBank    SettlementMethod = "bank-account"
	SettlementToEwallet SettlementMethod = "e-wallet"
)

// Settlement is a settlement batch.
//
// It reports totals only. There is no list of the transactions it covers, which is the
// gap a settlement file would have closed: the rows have to be fetched separately from
// each product's transaction list, filtered on settle_at within StartDate..EndDate.
type Settlement struct {
	ID          int64            `json:"id"`
	ReferenceNo string           `json:"reference_no"`
	Title       string           `json:"title"`
	Status      string           `json:"status"`
	Type        string           `json:"settlement_type"`
	Method      SettlementMethod `json:"settlement_method"`
	AutoCreated bool             `json:"is_auto_created"`

	StartDate TextTime `json:"start_date"`
	EndDate   TextTime `json:"end_date"`

	Amount          Amount `json:"amount"`
	TotalAdminFee   Amount `json:"total_admin_fee"`
	TotalVendorFee  Amount `json:"total_vendor_fee"`
	TotalOurMargin  Amount `json:"total_our_margin"`
	SettlementFee   Amount `json:"settlement_fee"`
	TotalToTransfer Amount `json:"total_to_transfer"`
	TotalRefunded   Amount `json:"total_refunded"`
	Currency        string `json:"currency"`

	TransferStatus string   `json:"transfer_status"`
	ApprovedBy     string   `json:"approved_by"`
	ApprovedAt     TextTime `json:"approved_at"`

	Recipient struct {
		BankCode      string `json:"bank_code"`
		AccountNumber string `json:"account_number"`
		AccountName   string `json:"account_name"`
	} `json:"recipient"`
}

// SettlementRefund is one transaction pulled back out of a settled balance.
type SettlementRefund struct {
	SettlementDetailID int64  `json:"settlement_detail_id"`
	TransactionType    string `json:"transaction_type"`
	TransactionID      int64  `json:"transaction_id"`
	TransactionReff    string `json:"transaction_reff"`
	AccountID          int64  `json:"account_id"`
	NetAmount          Amount `json:"net_amount"`

	IsRefunded        bool `json:"is_refunded"`
	IsRefundCancelled bool `json:"is_refund_cancelled"`

	RefundedBy        string   `json:"refunded_by"`
	RefundedAt        TextTime `json:"refunded_at"`
	RefundCancelledBy string   `json:"refund_cancelled_by"`
	RefundCancelledAt TextTime `json:"refund_cancelled_at"`
	Actor             string   `json:"actor"`
}

// SettlementNotification is a settlement lifecycle callback.
type SettlementNotification struct {
	Status    int             `json:"status"`
	Success   bool            `json:"success"`
	Event     SettlementEvent `json:"event"`
	Timestamp TextTime        `json:"timestamp"`

	Data struct {
		Settlement        Settlement        `json:"settlement"`
		TotalTransactions int               `json:"total_transactions"`
		Refund            *SettlementRefund `json:"refund"`
	} `json:"data"`
}

// ParseSettlementNotification decodes a verified settlement webhook body.
func ParseSettlementNotification(body []byte) (*SettlementNotification, error) {
	var n SettlementNotification
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, fmt.Errorf("singapay: cannot parse settlement webhook: %w", err)
	}
	return &n, nil
}

// MovesPendingToAvailable reports whether this batch converted pending balance into
// available balance, as opposed to paying out to a bank account.
func (s *Settlement) MovesPendingToAvailable() bool {
	return s.Method == SettlementToBalance || s.Method == SettlementAutoBalance
}
