package ledger

import (
	"context"
	"fmt"
	"strings"

	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// PaymentGateway is the slice of the Singapay API this ledger uses.
//
// It exists as an interface for one reason: LedgerClient decides what happens to money
// from what the gateway answers, and those decisions have to be testable without a live
// merchant account. *singapay.Client satisfies it, and NewLedgerClient takes it directly.
//
// The methods are grouped by what they are for, not by Singapay's API versions — accounts
// and balances are v1.0, disbursement is v2.0, and mixing them is expected rather than a
// migration artefact. See the singapay package docs.
type PaymentGateway interface {
	// Accounts
	CreateAccount(ctx context.Context, req singapay.CreateAccountRequest) (*singapay.Account, error)
	GetAccount(ctx context.Context, id string) (*singapay.Account, error)
	GetAccountBalance(ctx context.Context, accountID string) (*singapay.Balance, error)

	// Money in. Which one is used depends on the channel the caller asked for; see
	// paymentChannelKind.
	CreateVirtualAccount(ctx context.Context, accountID string, req singapay.CreateVirtualAccountRequest) (*singapay.VirtualAccount, error)
	GenerateQRIS(ctx context.Context, accountID string, req singapay.GenerateQRISRequest) (*singapay.QRISTransaction, error)
	CreateEwalletOrder(ctx context.Context, req singapay.CreateEwalletOrderRequest) (*singapay.EwalletTransaction, error)
	CreatePaymentLink(ctx context.Context, accountID string, req singapay.CreatePaymentLinkRequest) (*singapay.PaymentLink, error)
	// ListPaymentMethods reads the payment-link catalogue. A card payment is a payment
	// link pinned to the catalogue's card methods, and this is where their codes come from.
	ListPaymentMethods(ctx context.Context) ([]singapay.PaymentMethod, error)

	// Window listings. These predate the per-transaction settling pass, which reads one
	// invoice at a time instead (see the per-transaction reads below) and never filters
	// on a settlement window. Kept for operators and smoke tests that need to sweep a
	// period; nothing in the settlement path calls them.
	ListVATransactions(ctx context.Context, accountID string, w singapay.SettlementWindow) ([]singapay.VATransaction, singapay.Pagination, error)

	// The per-transaction reads. These are what the settlement pass actually uses: it
	// asks about each invoice it is waiting on rather than replaying a batch's window,
	// which is why the window's undocumented timezone stops mattering.
	//
	// GetVATransactionsByVANumber is the odd one out, and has to be: a VA transaction's
	// own id is not known until someone pays, so the VA number — known at creation — is
	// the only key a payment record can offer.
	GetVATransaction(ctx context.Context, accountID, transactionID string) (*singapay.VATransaction, error)
	GetVATransactionsByVANumber(ctx context.Context, accountID, vaNumber string) ([]singapay.VATransaction, singapay.Pagination, error)
	GetQRISTransaction(ctx context.Context, accountID string, id int64) (*singapay.QRISTransaction, error)
	GetEwalletTransaction(ctx context.Context, accountID, transactionID string) (*singapay.EwalletTransaction, error)
	GetPaymentLinkHistory(ctx context.Context, accountID string, historyID int64) (*singapay.PaymentLinkHistory, error)
	ListQRISTransactions(ctx context.Context, accountID string, w singapay.SettlementWindow) ([]singapay.QRISTransaction, singapay.Pagination, error)
	ListEwalletTransactions(ctx context.Context, accountID string, w singapay.SettlementWindow) ([]singapay.EwalletTransaction, singapay.Pagination, error)
	ListPaymentLinkHistories(ctx context.Context, accountID string, w singapay.SettlementWindow) ([]singapay.PaymentLinkHistory, singapay.Pagination, error)

	// Money out
	CheckBeneficiary(ctx context.Context, bankCode, accountNumber string) (*singapay.Beneficiary, error)
	CheckFee(ctx context.Context, accountID, bankSwiftCode string, netAmount int64) (*singapay.FeeQuote, error)
	Disburse(ctx context.Context, req singapay.DisburseRequest) (*singapay.Disbursement, error)
	InquiryDisbursement(ctx context.Context, accountID, referenceNumber string) (*singapay.Disbursement, error)

	// Transfers between the merchant's own sub-accounts, which is how a platform fee
	// leaves a seller's balance.
	TransferBetweenAccounts(ctx context.Context, fromAccountID string, req singapay.TransferRequest) (*singapay.AccountTransfer, error)

	// VerifyWebhook checks an inbound delivery's signature. Every webhook entry point
	// on LedgerClient calls it before reading a single field.
	VerifyWebhook(req singapay.WebhookRequest) error
}

// Compile-time proof that the real client satisfies the interface. Without this, a
// signature drift in the singapay package would only surface at the call site in a host
// application.
var _ PaymentGateway = (*singapay.Client)(nil)

// channelKind is which Singapay money-in product a fee-config channel maps to.
type channelKind int

const (
	channelUnknown channelKind = iota
	channelVirtualAccount
	channelQRIS
	channelEwallet
	// channelPaymentLink is the fallback when no channel was requested: the payer picks
	// one on Singapay's hosted page. It is a deliberate last resort — a payment link
	// reports no per-transaction fee anywhere, so anything paid through one cannot have
	// its fee reconciled. See domain.SettledTransaction.FeeReported.
	channelPaymentLink
	// channelCard is a card payment, issued as a payment link pinned to the catalogue's card
	// methods. Singapay's own card API is not used: it takes the card number and CVV in the
	// request body, which would put them on the merchant's servers. The hosted page takes
	// them instead, 3-D Secure included. The price is the payment link's blind spot — no
	// per-transaction fee is reported, so a card payment's fee is never reconciled either.
	channelCard
)

// ChannelPaymentLink is the channel recorded on a transaction whose payer chose the
// channel themselves. It is not a Singapay code and is never sent to the API.
const ChannelPaymentLink = "PAYMENT_LINK"

// ChannelCreditCard is the fee-config channel for card payments, and what a card payment
// records as its channel.
//
// Unlike the other channels it is not sent to Singapay as-is. The link is pinned to
// whatever the catalogue files under the card group (see cardPaymentMethodCodes), because
// which card methods a merchant is enabled for is Singapay's to say, and its hosted page
// finds them by group rather than by one fixed code.
const ChannelCreditCard = "CREDIT_CARD"

// minCardPayment is the smallest card charge Singapay accepts, in rupiah. Its card product
// states a minimum of IDR 10,000; a smaller link would be refused at creation with a
// validation error that names nothing the payer can act on.
const minCardPayment int64 = 10000

// paymentChannelKind resolves a fee-config payment channel to the Singapay product that
// issues it.
//
// The codes are Singapay's own, from GET /payment-link-manage/payment-methods, and they
// are the only spellings any Singapay endpoint accepts. An empty channel means the caller
// did not pin one, which routes to a payment link. CREDIT_CARD is ours, and routes to a
// payment link pinned to the card methods; see ChannelCreditCard.
func paymentChannelKind(channel string) channelKind {
	switch {
	case channel == "":
		return channelPaymentLink
	case channel == ChannelPaymentLink:
		return channelPaymentLink
	case channel == ChannelCreditCard:
		return channelCard
	case channel == "QRIS":
		return channelQRIS
	case strings.HasPrefix(channel, "VA_"):
		return channelVirtualAccount
	case strings.HasPrefix(channel, "EWALLET_"):
		return channelEwallet
	default:
		return channelUnknown
	}
}

// vaBankFromChannel extracts the bank from a VA_* channel code.
//
// Singapay's catalogue spells the channel "VA_BCA" but the create-VA body wants the bank
// alone, "BCA". Getting this wrong is a validation error rather than a silent
// misconfiguration, but it happens on the booking path, so it is checked here against the
// banks Singapay actually issues for.
func vaBankFromChannel(channel string) (singapay.VABank, error) {
	bank := singapay.VABank(strings.TrimPrefix(channel, "VA_"))
	switch bank {
	case singapay.BankBCA, singapay.BankBNI, singapay.BankBRI, singapay.BankMandiri,
		singapay.BankPermata, singapay.BankMaybank, singapay.BankCIMB, singapay.BankBSI,
		singapay.BankMuamalat, singapay.BankBNC, singapay.BankOCBC, singapay.BankDanamon:
		return bank, nil
	default:
		return "", fmt.Errorf("payment channel %q does not name a bank Singapay issues virtual accounts for", channel)
	}
}
