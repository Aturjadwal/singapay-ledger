package singapay

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// BeneficiaryBank identifies the destination of a disbursement.
type BeneficiaryBank struct {
	Code          string `json:"code"`
	Name          string `json:"name"`
	AccountName   string `json:"account_name"`
	AccountNumber string `json:"account_number"`
}

// Disbursement is a bank payout as the v2.0 endpoints describe it.
//
// Status is the field that decides what happens to a reserved balance; see
// [TransactionStatus.Terminal]. FailedCode and FailedReason are set only when Status
// is [StatusFailed].
type Disbursement struct {
	TransactionID   string            `json:"transaction_id"`
	ReferenceNumber string            `json:"reference_number"`
	Status          transactionStatus `json:"transaction_status"`

	Bank BeneficiaryBank `json:"bank"`

	// GrossAmount is what leaves the account: NetAmount plus Fee.
	GrossAmount Amount `json:"gross_amount"`
	Fee         Amount `json:"fee"`
	// NetAmount is what the beneficiary receives — the amount that was requested.
	NetAmount    Amount `json:"net_amount"`
	BalanceAfter Amount `json:"balance_after"`

	Notes string `json:"notes"`

	PostedAt    MillisTime `json:"post_timestamp"`
	ProcessedAt MillisTime `json:"processed_timestamp"`

	FailedCode   string `json:"failed_code"`
	FailedReason string `json:"failed_reason"`
}

// transactionStatus is Singapay's {code, desc} pair.
type transactionStatus struct {
	Code TransactionStatus `json:"code"`
	Desc string            `json:"desc"`
}

// TransactionStatus returns the payout's status code.
func (d *Disbursement) TransactionStatus() TransactionStatus { return d.Status.Code }

// DisburseRequest is the body of POST /api/v2.0/disbursement/transfer.
type DisburseRequest struct {
	// AccountID is the sub-account ULID to debit. On v2 it lives in the body, not the
	// path.
	AccountID string `json:"account_id"`

	// ReferenceNumber is the idempotency key, unique per account. Re-sending one
	// returns SP004 — see [Client.Disburse] for what to do about that.
	ReferenceNumber string `json:"reference_number"`

	// BankCode accepts a three-digit national code ("002") or a SWIFT code
	// ("BRINIDJA"). Storing SWIFT is the safer choice: [Client.CheckFee] is a v1.0
	// endpoint with no v2 counterpart and accepts SWIFT only.
	BankCode          string `json:"bank_code"`
	BankAccountNumber string `json:"bank_account_number"`

	// Amount is the NET amount in whole rupiah — what the beneficiary receives. The
	// transfer fee is added on top, so the account is debited Amount + fee.
	//
	// A caller that reserves only Amount will drift from Singapay's balance by the fee
	// on every payout. Quote with [Client.CheckFee] and reserve the gross.
	Amount int64 `json:"amount"`

	Notes string `json:"notes,omitempty"`
}

// Disburse submits a bank payout. The request is signed.
//
// SP000 means the instruction was accepted, not that money moved: read
// [Disbursement.TransactionStatus] and treat 01, 02 and 03 as still in flight.
//
// On failure, ask the returned error what it implies before touching a reserved balance:
//
//	if e, ok := singapay.AsError(err); ok {
//	    switch e.Outcome() {
//	    case singapay.OutcomeRefused:   // safe to release the reservation
//	    case singapay.OutcomeDuplicate: // InquiryDisbursement on the same reference
//	    case singapay.OutcomeUnknown:   // leave it reserved and inquire
//	    }
//	}
//
// Deciding this from the HTTP status instead is the trap: Singapay returns 400 for SP001,
// SP002, SP004 and SP005, and every one of those may still settle.
func (c *Client) Disburse(ctx context.Context, req DisburseRequest) (*Disbursement, error) {
	var out Disbursement
	if err := c.call(ctx, http.MethodPost, "/api/v2.0/disbursement/transfer", req, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

type inquiryStatusRequest struct {
	ReferenceNumber string `json:"reference_number"`
}

// InquiryDisbursement resolves a payout by the reference the merchant supplied. For a
// pending transfer, Singapay may refresh from the banking partner before answering.
//
// This is the endpoint that answers every non-terminal status and every
// [OutcomeUnknown] or [OutcomeDuplicate] failure.
func (c *Client) InquiryDisbursement(ctx context.Context, accountID, referenceNumber string) (*Disbursement, error) {
	var out Disbursement
	path := "/api/v2.0/disbursement/" + accountID + "/inquiry-status"
	if err := c.call(ctx, http.MethodPost, path, inquiryStatusRequest{ReferenceNumber: referenceNumber}, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// BeneficiaryStatus reports whether a bank account inquiry found the account.
type BeneficiaryStatus string

const (
	BeneficiaryValid   BeneficiaryStatus = "valid"
	BeneficiaryInvalid BeneficiaryStatus = "invalid"
)

// Beneficiary is the result of a bank account name inquiry.
type Beneficiary struct {
	BankCode      string            `json:"bank_code"`
	BankName      string            `json:"bank_name"`
	AccountNumber string            `json:"bank_account_number"`
	AccountName   string            `json:"bank_account_name"`
	Status        BeneficiaryStatus `json:"status"`
	// Message carries the bank's reason when Status is invalid.
	Message string `json:"message"`
}

// IsValid reports whether the account was found.
func (b *Beneficiary) IsValid() bool { return b.Status == BeneficiaryValid }

type checkBeneficiaryRequest struct {
	BankCode          string `json:"bank_code"`
	BankAccountNumber string `json:"bank_account_number"`
}

// CheckBeneficiary performs a real-time bank account name inquiry.
//
// An account that does not exist is *not* an error: Singapay answers HTTP 200 with
// SP000 and Status "invalid". Callers must read [Beneficiary.IsValid] rather than
// inferring validity from err == nil.
func (c *Client) CheckBeneficiary(ctx context.Context, bankCode, accountNumber string) (*Beneficiary, error) {
	var out Beneficiary
	req := checkBeneficiaryRequest{BankCode: bankCode, BankAccountNumber: accountNumber}
	if err := c.call(ctx, http.MethodPost, "/api/v2.0/disbursement/check-beneficiary", req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// FeeQuote is what a payout would cost, from POST .../check-fee.
type FeeQuote struct {
	// GrossAmount is what would be debited: NetAmount plus TransferFee.
	GrossAmount Amount `json:"gross_amount"`
	TransferFee Amount `json:"transfer_fee"`
	// NetAmount echoes the requested amount.
	NetAmount Amount `json:"net_amount"`
	Currency  string `json:"currency"`

	Beneficiary struct {
		FullName  string `json:"full_name"`
		ShortName string `json:"short_name"`
	} `json:"beneficiary"`
}

type checkFeeRequest struct {
	BankSwiftCode string `json:"bank_swift_code"`
	Amount        int64  `json:"amount"`
}

// CheckFee quotes the fee and total debit for a payout before it is submitted.
//
// It is a v1.0 endpoint and has no v2 counterpart, and it takes a SWIFT code where
// [DisburseRequest] takes either a SWIFT or a three-digit code. Callers holding a
// three-digit code cannot quote — which is the reason to store SWIFT everywhere.
//
// The quote fails if the resulting gross would exceed the account's available balance,
// so a successful quote is also a balance check.
func (c *Client) CheckFee(ctx context.Context, accountID, bankSwiftCode string, netAmount int64) (*FeeQuote, error) {
	var out FeeQuote
	path := "/api/v1.0/disbursement/" + accountID + "/check-fee"
	req := checkFeeRequest{BankSwiftCode: bankSwiftCode, Amount: netAmount}
	if err := c.call(ctx, http.MethodPost, path, req, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// DisbursementRecord is a payout as the v1.0 list and show endpoints describe it.
//
// It differs from [Disbursement] in more than formatting: Status here is a free-text
// lifecycle string ("pending", "success", "failed") rather than a two-digit code, so it
// cannot distinguish refunded or cancelled from failed. Prefer [Client.InquiryDisbursement]
// when the distinction matters; these two endpoints exist because v2.0 has no list or show
// at all.
type DisbursementRecord struct {
	TransactionID   string          `json:"transaction_id"`
	ReferenceNumber string          `json:"reference_number"`
	Status          string          `json:"status"`
	Bank            BeneficiaryBank `json:"bank"`

	GrossAmount  Amount `json:"gross_amount"`
	Fee          Amount `json:"fee"`
	NetAmount    Amount `json:"net_amount"`
	BalanceAfter Amount `json:"balance_after"`

	Notes string `json:"notes"`

	PostedAt    MillisTime `json:"post_timestamp"`
	ProcessedAt MillisTime `json:"processed_timestamp"`
}

// GetDisbursement reads one payout by its Singapay transaction id — not by the merchant
// reference, and not by any database key.
func (c *Client) GetDisbursement(ctx context.Context, accountID, transactionID string) (*DisbursementRecord, error) {
	var out DisbursementRecord
	path := "/api/v1.0/disbursement/" + accountID + "/" + transactionID
	if err := c.call(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListDisbursements returns an account's payouts, newest first. Singapay limits this to
// the last twelve months, 25 rows per page.
func (c *Client) ListDisbursements(ctx context.Context, accountID string, page int) ([]DisbursementRecord, Pagination, error) {
	path := "/api/v1.0/disbursement/" + accountID
	if page > 0 {
		path += "?" + url.Values{"page": {strconv.Itoa(page)}}.Encode()
	}

	var out []DisbursementRecord
	var pg Pagination
	if err := c.callPaged(ctx, http.MethodGet, path, nil, &out, &pg); err != nil {
		return nil, Pagination{}, err
	}
	return out, pg, nil
}
