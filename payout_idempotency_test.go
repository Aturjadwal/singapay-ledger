package ledger

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/ledgererr"
	"github.com/21strive/ledger/repo"
	"github.com/21strive/ledger/singapay"
)

// FakeDisbursementRepository is an in-memory DisbursementRepository. Save mirrors the
// Postgres one in the way that matters here: payout_request_id and gateway_fee are written
// once and never overwritten, so a test can prove a retry reuses the reference it was
// given rather than inventing one.
type FakeDisbursementRepository struct {
	disbursements map[string]*domain.Disbursement
}

var _ domain.DisbursementRepository = (*FakeDisbursementRepository)(nil)

func NewFakeDisbursementRepository() *FakeDisbursementRepository {
	return &FakeDisbursementRepository{disbursements: make(map[string]*domain.Disbursement)}
}

func (f *FakeDisbursementRepository) Save(ctx context.Context, d *domain.Disbursement) error {
	if existing, ok := f.disbursements[d.UUID]; ok {
		stored := *d
		stored.PayoutRequestID = existing.PayoutRequestID
		stored.GatewayFee = existing.GatewayFee
		f.disbursements[d.UUID] = &stored
		return nil
	}
	stored := *d
	f.disbursements[d.UUID] = &stored
	return nil
}

func (f *FakeDisbursementRepository) GetByID(ctx context.Context, id string) (*domain.Disbursement, error) {
	if d, ok := f.disbursements[id]; ok {
		copied := *d
		return &copied, nil
	}
	return nil, repo.ErrNotFound
}

func (f *FakeDisbursementRepository) GetByPayoutRequestID(ctx context.Context, payoutRequestID string) (*domain.Disbursement, error) {
	if payoutRequestID == "" {
		return nil, repo.ErrNotFound
	}
	for _, d := range f.disbursements {
		if d.PayoutRequestID == payoutRequestID {
			copied := *d
			return &copied, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakeDisbursementRepository) GetByLedgerID(ctx context.Context, ledgerID string, page, pageSize int) ([]*domain.Disbursement, error) {
	return nil, nil
}

func (f *FakeDisbursementRepository) GetByAccountIDWithCursor(ctx context.Context, accountID string, cursor string, pageSize int, sortOrder string) ([]*domain.Disbursement, error) {
	return nil, nil
}

func (f *FakeDisbursementRepository) GetPendingByLedgerID(ctx context.Context, ledgerID string) ([]*domain.Disbursement, error) {
	return nil, nil
}

func (f *FakeDisbursementRepository) GetPendingOlderThan(ctx context.Context, cutoff time.Time, limit int) ([]*domain.Disbursement, error) {
	var pending []*domain.Disbursement
	for _, d := range f.disbursements {
		if d.IsPending() && d.CreatedAt.Before(cutoff) {
			copied := *d
			pending = append(pending, &copied)
		}
	}
	return pending, nil
}

func (f *FakeDisbursementRepository) UpdateStatus(ctx context.Context, id string, status domain.DisbursementStatus, processedAt *time.Time, failureReason string) error {
	if d, ok := f.disbursements[id]; ok {
		d.Status = status
		d.ProcessedAt = processedAt
		d.FailureReason = failureReason
		return nil
	}
	return repo.ErrNotFound
}

// ═══════════════════════════════════════════════════════════════════════════
// Fake gateway
// ═══════════════════════════════════════════════════════════════════════════

// fakeGateway is a scripted PaymentGateway. Only the money-out surface is exercised here;
// the rest satisfies the interface and panics if a test reaches it by accident, which is
// more useful than a silent zero value on a path that moves money.
type fakeGateway struct {
	// references records every reference_number a payout was sent under, in order. It is
	// the whole point of the idempotency tests: a retry must reuse, never mint.
	references []string
	bodies     []singapay.DisburseRequest

	// beforeCall runs at the moment the gateway would be hit, so a test can inspect what
	// the ledger had already written by then.
	beforeCall func()

	// disburse is what Disburse returns. err takes precedence when set.
	disburse *singapay.Disbursement
	err      error

	// fee is what CheckFee quotes. feeErr makes the quote fail, which must not fail the
	// withdrawal.
	fee    int64
	feeErr error

	// inquiry is what InquiryDisbursement returns. Default is a not-found error, which is
	// the only answer that makes re-sending safe.
	inquiry    *singapay.Disbursement
	inquiryErr error
	inquiries  int
}

var _ PaymentGateway = (*fakeGateway)(nil)

func (f *fakeGateway) Disburse(ctx context.Context, req singapay.DisburseRequest) (*singapay.Disbursement, error) {
	if f.beforeCall != nil {
		f.beforeCall()
	}
	f.references = append(f.references, req.ReferenceNumber)
	f.bodies = append(f.bodies, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.disburse, nil
}

func (f *fakeGateway) CheckFee(ctx context.Context, accountID, bankSwiftCode string, netAmount int64) (*singapay.FeeQuote, error) {
	if f.feeErr != nil {
		return nil, f.feeErr
	}
	return &singapay.FeeQuote{
		TransferFee: singapay.NewAmount(f.fee, "IDR"),
		NetAmount:   singapay.NewAmount(netAmount, "IDR"),
		GrossAmount: singapay.NewAmount(netAmount+f.fee, "IDR"),
	}, nil
}

func (f *fakeGateway) InquiryDisbursement(ctx context.Context, accountID, referenceNumber string) (*singapay.Disbursement, error) {
	f.inquiries++
	if f.inquiry != nil {
		return f.inquiry, nil
	}
	if f.inquiryErr != nil {
		return nil, f.inquiryErr
	}
	return nil, &singapay.Error{StatusCode: http.StatusNotFound, Code: singapay.CodeTransactionNotFound, Message: "transaction not found"}
}

func (f *fakeGateway) CreateAccount(context.Context, singapay.CreateAccountRequest) (*singapay.Account, error) {
	panic("fakeGateway.CreateAccount: not scripted for this test")
}
func (f *fakeGateway) GetAccount(context.Context, string) (*singapay.Account, error) {
	panic("fakeGateway.GetAccount: not scripted for this test")
}
func (f *fakeGateway) GetAccountBalance(context.Context, string) (*singapay.Balance, error) {
	panic("fakeGateway.GetAccountBalance: not scripted for this test")
}
func (f *fakeGateway) CreateVirtualAccount(context.Context, string, singapay.CreateVirtualAccountRequest) (*singapay.VirtualAccount, error) {
	panic("fakeGateway.CreateVirtualAccount: not scripted for this test")
}
func (f *fakeGateway) GenerateQRIS(context.Context, string, singapay.GenerateQRISRequest) (*singapay.QRISTransaction, error) {
	panic("fakeGateway.GenerateQRIS: not scripted for this test")
}
func (f *fakeGateway) CreateEwalletOrder(context.Context, singapay.CreateEwalletOrderRequest) (*singapay.EwalletTransaction, error) {
	panic("fakeGateway.CreateEwalletOrder: not scripted for this test")
}
func (f *fakeGateway) CreatePaymentLink(context.Context, string, singapay.CreatePaymentLinkRequest) (*singapay.PaymentLink, error) {
	panic("fakeGateway.CreatePaymentLink: not scripted for this test")
}
func (f *fakeGateway) ListVATransactions(context.Context, string, singapay.SettlementWindow) ([]singapay.VATransaction, singapay.Pagination, error) {
	panic("fakeGateway.ListVATransactions: not scripted for this test")
}
func (f *fakeGateway) ListQRISTransactions(context.Context, string, singapay.SettlementWindow) ([]singapay.QRISTransaction, singapay.Pagination, error) {
	panic("fakeGateway.ListQRISTransactions: not scripted for this test")
}
func (f *fakeGateway) ListEwalletTransactions(context.Context, string, singapay.SettlementWindow) ([]singapay.EwalletTransaction, singapay.Pagination, error) {
	panic("fakeGateway.ListEwalletTransactions: not scripted for this test")
}
func (f *fakeGateway) ListPaymentLinkHistories(context.Context, string, singapay.SettlementWindow) ([]singapay.PaymentLinkHistory, singapay.Pagination, error) {
	panic("fakeGateway.ListPaymentLinkHistories: not scripted for this test")
}
func (f *fakeGateway) CheckBeneficiary(context.Context, string, string) (*singapay.Beneficiary, error) {
	panic("fakeGateway.CheckBeneficiary: not scripted for this test")
}
func (f *fakeGateway) TransferBetweenAccounts(context.Context, string, singapay.TransferRequest) (*singapay.AccountTransfer, error) {
	panic("fakeGateway.TransferBetweenAccounts: not scripted for this test")
}
func (f *fakeGateway) VerifyWebhook(singapay.WebhookRequest) error {
	panic("fakeGateway.VerifyWebhook: not scripted for this test")
}

// payoutWithStatus builds a Singapay disbursement carrying a given two-digit transaction
// status.
//
// It goes through JSON rather than a struct literal because the status field's type is
// unexported — which is the right design for the client and means these tests exercise the
// real decoding path instead of a shape invented alongside it.
func payoutWithStatus(t *testing.T, code string) *singapay.Disbursement {
	t.Helper()

	var d singapay.Disbursement
	body := `{
		"transaction_id": "SP-TX-1",
		"reference_number": "ref-1",
		"transaction_status": {"code": "` + code + `", "desc": "scripted"},
		"gross_amount": "50000.00",
		"net_amount": "50000.00",
		"fee": "0.00"
	}`
	require.NoError(t, json.Unmarshal([]byte(body), &d))
	return &d
}

// payoutSuccess is a completed payout (status 00).
func payoutSuccess(t *testing.T) *singapay.Disbursement { return payoutWithStatus(t, "00") }

// ═══════════════════════════════════════════════════════════════════════════
// Fixtures
// ═══════════════════════════════════════════════════════════════════════════

func newPayoutTestClient(t *testing.T, gw *fakeGateway, available int64) (*LedgerClient, *FakeRepositoryProvider, *domain.Account) {
	t.Helper()

	fakes := NewFakeRepositoryProvider()
	ctx := context.Background()

	account := domain.NewSellerAccount("01SELLERACCOUNTULID", "seller-1", domain.CurrencyIDR)
	account.SetSingapayAccount("01SELLERACCOUNTULID", "000000000123")
	require.NoError(t, fakes.Account().Save(ctx, &account))

	if available > 0 {
		journal := domain.NewJournal(domain.EventTypePaymentSuccess, domain.SourceTypeProductTransaction, "seed", nil)
		require.NoError(t, fakes.Journal().Save(ctx, journal))
		require.NoError(t, fakes.LedgerEntry().Save(ctx, &domain.LedgerEntry{
			JournalUUID:   journal.UUID,
			AccountUUID:   account.UUID,
			Amount:        available,
			BalanceBucket: domain.BalanceBucketAvailable,
			EntryType:     domain.EntryTypeSettlement,
			SourceType:    domain.SourceTypeProductTransaction,
			SourceID:      "seed",
		}))
	}

	client := &LedgerClient{
		txProvider:   NewFakeTransactionProvider(fakes),
		repoProvider: fakes,
		logger:       testLogger(),
		gateway:      gw,
	}

	return client, fakes, &account
}

func withdrawRequest() *WithdrawRequest {
	return &WithdrawRequest{
		AccountID:     "seller-1",
		Amount:        50000,
		Currency:      "IDR",
		BankCode:      "BNINIDJA",
		AccountNumber: "712739123020001",
		AccountName:   "Ria Florensi",
	}
}

func countDebits(fakes *FakeRepositoryProvider) int {
	n := 0
	for _, e := range fakes.ledgerEntryRepo.entries {
		if e.EntryType == domain.EntryTypeDisbursement {
			n++
		}
	}
	return n
}

func countReversals(fakes *FakeRepositoryProvider) int {
	n := 0
	for _, e := range fakes.ledgerEntryRepo.entries {
		if e.EntryType == domain.EntryTypeDisbursementReversal {
			n++
		}
	}
	return n
}

func availableBalance(fakes *FakeRepositoryProvider, accountUUID string) int64 {
	_, available, _ := fakes.ledgerEntryRepo.GetAllBalances(context.Background(), accountUUID)
	return available
}

// ═══════════════════════════════════════════════════════════════════════════
// Tests
// ═══════════════════════════════════════════════════════════════════════════

// The row and its reference must exist BEFORE the payout leaves. If the gateway were
// called first, a crash between the payout and the commit would lose the reference and the
// payout could never be asked about again.
func TestWithdraw_PersistsReferenceBeforeCallingTheGateway(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}

	var storedAtCallTime *domain.Disbursement
	client, fakes, _ := newPayoutTestClient(t, gw, 100000)
	gw.beforeCall = func() {
		for _, d := range fakes.disbursementRepo.disbursements {
			copied := *d
			storedAtCallTime = &copied
		}
	}

	resp, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)

	require.NotNil(t, storedAtCallTime, "no disbursement row existed when the gateway was called")
	assert.NotEmpty(t, storedAtCallTime.PayoutRequestID)

	// And the reference on the row is the reference Singapay actually saw.
	require.Len(t, gw.references, 1)
	assert.Equal(t, storedAtCallTime.PayoutRequestID, gw.references[0])
	assert.Equal(t, resp.DisbursementID, storedAtCallTime.UUID)
}

// Singapay's amount is the NET the beneficiary receives; the transfer fee is charged on
// top. Reserving only the net leaves the ledger short by the fee on every payout, and the
// drift is silent — the books stay internally consistent and disagree only with Singapay.
func TestWithdraw_ReservesTheGrossIncludingTheTransferFee(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t), fee: 4000}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	resp, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest()) // net 50000
	require.NoError(t, err)

	assert.Equal(t, int64(4000), resp.TransferFee)
	assert.Equal(t, int64(46000), availableBalance(fakes, account.UUID),
		"the reservation must hold net + fee (50000 + 4000), not just the net")
}

// A quote that cannot be made is not a withdrawal that cannot be made. check-fee accepts
// SWIFT codes only, so an account stored with a three-digit bank code can never be quoted —
// refusing those payouts outright would be worse than under-reserving by the fee.
func TestWithdraw_ProceedsWhenTheFeeCannotBeQuoted(t *testing.T) {
	gw := &fakeGateway{
		disburse: payoutSuccess(t),
		feeErr:   &singapay.Error{StatusCode: http.StatusBadRequest, Code: singapay.CodeValidationError, Message: "bank_swift_code required"},
	}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	resp, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.NoError(t, err)

	assert.Zero(t, resp.TransferFee)
	assert.Len(t, gw.references, 1, "the payout must still have gone out")
	assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID))
}

// Outcome, not the HTTP status, decides whether the reservation may be released. Singapay
// answers HTTP 400 for SP001, SP002, SP004 and SP005 and documents every one of them as
// "call inquiry-status" — releasing on a 4xx is exactly how a payout gets made twice.
func TestWithdraw_UnknownOutcomeKeepsTheReservationEvenOn4xx(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *singapay.Error
	}{
		{"SP001 transaction failure arrives as HTTP 400", &singapay.Error{StatusCode: http.StatusBadRequest, Code: singapay.CodeTransactionFailure}},
		{"SP005 timeout arrives as HTTP 400", &singapay.Error{StatusCode: http.StatusBadRequest, Code: singapay.CodeTimeout}},
		{"SP004 duplicate reference arrives as HTTP 400", &singapay.Error{StatusCode: http.StatusBadRequest, Code: singapay.CodeDuplicateReference}},
		{"a transport failure has no code at all", &singapay.Error{Err: context.DeadlineExceeded}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeGateway{err: tc.err}
			client, fakes, account := newPayoutTestClient(t, gw, 100000)

			_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
			require.Error(t, err)

			assert.Zero(t, countReversals(fakes), "an unknown outcome must not release the reservation")
			assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID),
				"the money is still committed to a payout that may yet settle")

			for _, d := range fakes.disbursementRepo.disbursements {
				assert.Equal(t, domain.DisbursementStatusPending, d.Status,
					"a payout that may still settle must stay replayable, and FAILED is terminal")
			}
		})
	}
}

// A refusal Singapay states outright is the one case where the money is known not to have
// moved, so the reservation goes back.
func TestWithdraw_RefusalReleasesTheReservation(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{
		StatusCode: http.StatusBadRequest,
		Code:       singapay.CodeBeneficiaryNotFound,
		Message:    "beneficiary account not found",
	}}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)

	assert.Equal(t, 1, countReversals(fakes))
	assert.Equal(t, int64(100000), availableBalance(fakes, account.UUID))
	for _, d := range fakes.disbursementRepo.disbursements {
		assert.Equal(t, domain.DisbursementStatusFailed, d.Status)
	}
}

// SP000 means the instruction was accepted, not that money moved. The two-digit transaction
// status is the answer, and 04, 05, 06 and 07 are terminal failures — reading them as
// still-in-flight would hold a seller's balance against a transfer that will never complete.
func TestWithdraw_TerminalTransactionStatusReleasesTheReservation(t *testing.T) {
	for _, code := range []string{"04", "05", "06", "07"} {
		t.Run("status "+code, func(t *testing.T) {
			gw := &fakeGateway{disburse: payoutWithStatus(t, code)}
			client, fakes, account := newPayoutTestClient(t, gw, 100000)

			_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
			require.NoError(t, err, "a refused payout is a completed request, not a failed call")

			assert.Equal(t, 1, countReversals(fakes))
			assert.Equal(t, int64(100000), availableBalance(fakes, account.UUID))
			for _, d := range fakes.disbursementRepo.disbursements {
				assert.Equal(t, domain.DisbursementStatusFailed, d.Status)
			}
		})
	}
}

// 01, 02 and 03 are accepted-and-moving. The money stays held.
func TestWithdraw_InFlightStatusKeepsTheReservation(t *testing.T) {
	for _, code := range []string{"01", "02", "03"} {
		t.Run("status "+code, func(t *testing.T) {
			gw := &fakeGateway{disburse: payoutWithStatus(t, code)}
			client, fakes, account := newPayoutTestClient(t, gw, 100000)

			_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
			require.NoError(t, err)

			assert.Zero(t, countReversals(fakes))
			assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID))
		})
	}
}

// The payoff of storing the reference: a retry asks about it instead of sending it again.
// Singapay answers SP004 for a reference it has already seen, so re-sending blind is both
// useless and dangerous — the original may well have succeeded.
func TestRetryDisbursement_InquiresInsteadOfResending(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{Err: context.DeadlineExceeded}}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)
	require.Len(t, gw.references, 1)
	firstReference := gw.references[0]

	var pending *domain.Disbursement
	for _, d := range fakes.disbursementRepo.disbursements {
		pending = d
	}
	require.NotNil(t, pending)

	// Singapay now says the payout it never answered about did in fact go through.
	gw.err = nil
	gw.inquiry = payoutSuccess(t)

	resp, err := client.RetryDisbursement(context.Background(), pending.UUID)
	require.NoError(t, err)

	assert.Equal(t, 1, gw.inquiries, "the retry must ask before it sends")
	assert.Len(t, gw.references, 1, "a payout that already exists must not be sent a second time")
	assert.Equal(t, string(domain.DisbursementStatusCompleted), resp.Status)
	assert.Equal(t, firstReference, pending.PayoutRequestID)
	assert.Equal(t, int64(50000), availableBalance(fakes, account.UUID))
}

// Not-found is the one inquiry answer that makes re-sending safe: Singapay has no record of
// the reference, so nothing can have been paid under it.
func TestRetryDisbursement_ResendsOnlyWhenTheReferenceIsUnknown(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{Err: context.DeadlineExceeded}}
	client, fakes, _ := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)
	firstReference := gw.references[0]

	var pending *domain.Disbursement
	for _, d := range fakes.disbursementRepo.disbursements {
		pending = d
	}

	gw.err = nil
	gw.disburse = payoutSuccess(t) // inquiry defaults to SP009 not-found

	_, err = client.RetryDisbursement(context.Background(), pending.UUID)
	require.NoError(t, err)

	require.Len(t, gw.references, 2)
	assert.Equal(t, firstReference, gw.references[1],
		"the retry must present the reference the row already carries, not a fresh one")
}

// An inquiry that fails for any other reason leaves the question open, and an open question
// is not a licence to send money again.
func TestRetryDisbursement_RefusesToResendOnAnInconclusiveInquiry(t *testing.T) {
	gw := &fakeGateway{err: &singapay.Error{Err: context.DeadlineExceeded}}
	client, fakes, _ := newPayoutTestClient(t, gw, 100000)

	_, err := client.Withdraw(context.Background(), "seller-1", withdrawRequest())
	require.Error(t, err)

	var pending *domain.Disbursement
	for _, d := range fakes.disbursementRepo.disbursements {
		pending = d
	}

	gw.err = nil
	gw.disburse = payoutSuccess(t)
	gw.inquiryErr = &singapay.Error{StatusCode: http.StatusBadGateway, Message: "upstream unavailable"}

	_, err = client.RetryDisbursement(context.Background(), pending.UUID)
	require.Error(t, err)
	assert.True(t, ledgererr.IsAppError(err, ledgererr.ErrGatewayOutcomeUnknown),
		"expected an unknown-outcome error, got: %v", err)
	assert.Len(t, gw.references, 1, "no second payout may be sent while the outcome is unknown")
}

// Terminal rows are refused outright: COMPLETED has been paid and booked, FAILED is a
// definite refusal. Neither may be replayed.
func TestRetryDisbursement_RefusesTerminalRows(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(d *domain.Disbursement)
	}{
		{"completed", func(d *domain.Disbursement) {
			_ = d.MarkProcessing("SP-TX-1")
			_ = d.MarkCompleted("SP-TX-1")
		}},
		{"failed", func(d *domain.Disbursement) { _ = d.MarkFailed("Singapay refused the payout") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &fakeGateway{disburse: payoutSuccess(t)}
			client, fakes, account := newPayoutTestClient(t, gw, 100000)

			d, err := domain.NewDisbursementWithID(domain.GenerateID(), account.UUID, 50000, domain.CurrencyIDR,
				domain.BankAccount{BankCode: "BNINIDJA", AccountNumber: "712739123020001", AccountName: "Ria Florensi"}, "")
			require.NoError(t, err)
			d.PayoutRequestID = "some-reference"
			tc.apply(d)
			require.NoError(t, fakes.Disbursement().Save(context.Background(), d))

			_, err = client.RetryDisbursement(context.Background(), d.UUID)
			require.Error(t, err)
			assert.Empty(t, gw.references, "the gateway must not be called at all for a row that cannot be safely replayed")
			assert.Zero(t, gw.inquiries)
		})
	}
}

// A row with no stored reference cannot be replayed at all. Minting one now would look like
// a protected retry while handing Singapay a reference it has never seen — so a payout that
// already went out would go out again.
func TestRetryDisbursement_RefusesARowWithNoReference(t *testing.T) {
	gw := &fakeGateway{disburse: payoutSuccess(t)}
	client, fakes, account := newPayoutTestClient(t, gw, 100000)

	d, err := domain.NewDisbursementWithID(domain.GenerateID(), account.UUID, 50000, domain.CurrencyIDR,
		domain.BankAccount{BankCode: "BNINIDJA", AccountNumber: "712739123020001", AccountName: "Ria Florensi"}, "")
	require.NoError(t, err)
	require.NoError(t, fakes.Disbursement().Save(context.Background(), d))

	_, err = client.RetryDisbursement(context.Background(), d.UUID)
	require.Error(t, err)
	assert.Empty(t, gw.references)
	assert.Zero(t, gw.inquiries)
}
