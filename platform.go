package ledger

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
)

// ─────────────────────────────────────────────────────────────────────────────
// Platform account
//
// The platform's own account collects the platform fee on every seller's sale and the
// proceeds of the platform's own sales, and pays out to the platform's bank accounts. The
// operations below are the seller's balance, withdrawal and history, with the account
// resolved by type instead of by seller id — there is exactly one, enforced by a partial
// unique index on owner_type, and it is provisioned by hand per environment.
// ─────────────────────────────────────────────────────────────────────────────

const (
	defaultPlatformTransactionsPageSize = 20
	maxPlatformTransactionsPageSize     = 100
)

// GetPlatformAccount returns the platform's account.
//
// Its balance fields are the cached figures kept in step with every ledger entry — the ones
// GetBalance reads for a seller — so a consumer showing the platform's balance reads them
// from here. ErrLedgerNotFound means the account has not been provisioned.
func (c *LedgerClient) GetPlatformAccount(ctx context.Context) (*domain.Account, error) {
	account, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get platform account", err)
	}
	return account, nil
}

// WithdrawFromPlatform pays out of the platform account to a bank account.
//
// It is Withdraw with the account resolved by type rather than by seller, and everything
// after the lookup is the same code: the transfer fee carved out of the requested amount,
// the balance reserved under a row lock, the reference stored before the call, the outcome
// booked by the one path shared by the transfer answer, an inquiry and the webhook. A
// platform payout is an ordinary disbursement row, so RetryDisbursement and
// HandleDisbursementNotification resolve it without being told whose it is; the outcome the
// webhook hands back says so in OwnerType.
//
// req.AccountID is ignored — there is one platform account.
//
// The reservation is checked against the platform's AVAILABLE ledger balance, and part of
// that can be platform fees still sitting in sellers' sub-accounts, waiting for
// ProcessPlatformFeeTransfer. A payout larger than the platform sub-account really holds is
// refused by Singapay (SP003, insufficient funds), which releases the reservation and books
// the disbursement FAILED — the same path as any other refusal.
func (c *LedgerClient) WithdrawFromPlatform(ctx context.Context, req *WithdrawRequest) (*WithdrawResponse, error) {
	if req.Amount <= 0 {
		return nil, ledgererr.ErrInvalidDisbursementAmount
	}

	account, err := c.GetPlatformAccount(ctx)
	if err != nil {
		return nil, err
	}

	return c.withdrawFrom(ctx, account, req)
}

// ─────────────────────────────────────────────────────────────────────────────
// Platform statement
// ─────────────────────────────────────────────────────────────────────────────

// PlatformTransactionType says which way money moved on the platform account.
type PlatformTransactionType string

const (
	// PlatformTransactionIncome is money in: the platform fee on a seller's sale, or the
	// proceeds of a sale the platform made itself.
	PlatformTransactionIncome PlatformTransactionType = "INCOME"

	// PlatformTransactionPayout is money out: a disbursement from the platform account.
	PlatformTransactionPayout PlatformTransactionType = "PAYOUT"
)

// PlatformIncomeSource says why a transaction credited the platform.
type PlatformIncomeSource string

const (
	// PlatformIncomeSourceFee is the platform's fee on a seller's sale. The payer's money
	// lands in the seller's Singapay sub-account, and the fee reaches the platform's only
	// when ProcessPlatformFeeTransfer sweeps it: until the transaction's
	// PlatformFeeTransferred is set, the platform's books hold money its sub-account does
	// not.
	PlatformIncomeSourceFee PlatformIncomeSource = "PLATFORM_FEE"

	// PlatformIncomeSourceSale is a sale the platform made itself — a subscription — paid
	// against the platform's own sub-account, so there is nothing to sweep.
	PlatformIncomeSourceSale PlatformIncomeSource = "PLATFORM_SALE"
)

// PlatformTransaction is one line of the platform account's statement.
type PlatformTransaction struct {
	Type PlatformTransactionType

	// ID is the product transaction's UUID for an income and the disbursement's for a
	// payout.
	ID string

	// OccurredAt is when the money moved as far as the platform's books are concerned:
	// when the payer paid, for an income; when the payout was requested, for a payout. It
	// is the key the statement is ordered by.
	OccurredAt time.Time

	// Amount is never negative. For an income it is what the platform was credited — the
	// sum of its ledger entries for the transaction, so a settled line carries the settled
	// figure, not the priced one. For a payout it is the requested amount, transfer fee
	// included: what the reservation held. A payout that FAILED or was CANCELLED had that
	// hold released again; read its Status.
	Amount int64

	// Income is set on an INCOME line and nil on a PAYOUT line.
	Income *PlatformIncomeDetail

	// Payout is set on a PAYOUT line and nil on an INCOME line.
	Payout *domain.Disbursement
}

// PlatformIncomeDetail is what an income line knows beyond its amount.
type PlatformIncomeDetail struct {
	Source PlatformIncomeSource

	// Transaction is the sale itself. Its Status says which balance bucket the credit sits
	// in: COMPLETED is PENDING, awaiting settlement; SETTLED is AVAILABLE.
	Transaction *domain.ProductTransaction
}

// PlatformTransactionsRequest pages through the platform statement.
type PlatformTransactionsRequest struct {
	// Type narrows the statement to one direction. Empty lists both, interleaved.
	Type PlatformTransactionType

	// Cursor is the previous page's NextCursor, or empty for the first page. It is opaque,
	// and only valid for the same Type and SortOrder it was issued under.
	Cursor string

	// PageSize defaults to 20 and is capped at 100.
	PageSize int

	// SortOrder is "ASC" or "DESC" on OccurredAt. Anything else is DESC, newest first.
	SortOrder string
}

// PlatformTransactionsResponse is one page of the platform statement.
type PlatformTransactionsResponse struct {
	Transactions []*PlatformTransaction

	// NextCursor continues after the last line of this page. Empty when HasMore is false.
	NextCursor string
	HasMore    bool
}

// GetPlatformTransactions returns a page of the platform account's statement: money in and
// money out, interleaved by when each happened.
//
// It is a pure read of this package's tables — no gateway call.
//
// The two directions live in different tables, so the page is a merge. Each side is read in
// (OccurredAt, ID) order, one line more than the page, continuing past the same cursor, and
// the two are interleaved in Go. The first PageSize+1 lines of the union are always among
// the first PageSize+1 of each side, so the page is exact, and the extra line is how
// HasMore is known without a count. The cursor carries its sort key rather than naming a
// row to look up, because a row named by the last page could live in either table.
//
// An invalid cursor is CodeInvalidRequest. ErrLedgerNotFound means the platform account has
// not been provisioned.
func (c *LedgerClient) GetPlatformTransactions(ctx context.Context, req PlatformTransactionsRequest) (*PlatformTransactionsResponse, error) {
	switch req.Type {
	case "", PlatformTransactionIncome, PlatformTransactionPayout:
	default:
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			fmt.Sprintf("unknown platform transaction type %q", req.Type), nil)
	}

	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultPlatformTransactionsPageSize
	}
	if pageSize > maxPlatformTransactionsPageSize {
		pageSize = maxPlatformTransactionsPageSize
	}
	ascending := req.SortOrder == "ASC"

	after, err := decodePlatformCursor(req.Cursor)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "invalid cursor", err)
	}

	account, err := c.GetPlatformAccount(ctx)
	if err != nil {
		return nil, err
	}

	var incomes []*PlatformTransaction
	if req.Type != PlatformTransactionPayout {
		rows, err := c.repoProvider.ProductTransaction().GetPlatformIncomes(ctx, account.UUID, after, pageSize+1, ascending)
		if err != nil {
			return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to list platform incomes", err)
		}
		for _, row := range rows {
			incomes = append(incomes, platformIncomeLine(account.UUID, row))
		}
	}

	var payouts []*PlatformTransaction
	if req.Type != PlatformTransactionIncome {
		rows, err := c.repoProvider.Disbursement().GetByAccountIDAfter(ctx, account.UUID, after, pageSize+1, ascending)
		if err != nil {
			return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to list platform payouts", err)
		}
		for _, row := range rows {
			payouts = append(payouts, platformPayoutLine(row))
		}
	}

	lines := mergePlatformTransactions(incomes, payouts, ascending)

	resp := &PlatformTransactionsResponse{Transactions: lines}
	if len(lines) > pageSize {
		resp.Transactions = lines[:pageSize]
		resp.HasMore = true

		last := resp.Transactions[pageSize-1]
		resp.NextCursor = encodePlatformCursor(domain.KeysetCursor{At: last.OccurredAt, ID: last.ID})
	}

	return resp, nil
}

// platformIncomeLine turns a paid transaction into a statement line.
//
// OccurredAt must be computed exactly as the repository orders by — completed_at, falling
// back to created_at — or the cursor built from it would not continue where the page ended.
func platformIncomeLine(platformAccountUUID string, income *domain.PlatformIncome) *PlatformTransaction {
	tx := income.Transaction

	occurredAt := tx.CreatedAt
	if tx.CompletedAt != nil {
		occurredAt = *tx.CompletedAt
	}

	source := PlatformIncomeSourceFee
	if tx.SellerAccountID == platformAccountUUID {
		source = PlatformIncomeSourceSale
	}

	return &PlatformTransaction{
		Type:       PlatformTransactionIncome,
		ID:         tx.UUID,
		OccurredAt: occurredAt,
		Amount:     income.PlatformAmount,
		Income: &PlatformIncomeDetail{
			Source:      source,
			Transaction: tx,
		},
	}
}

func platformPayoutLine(d *domain.Disbursement) *PlatformTransaction {
	return &PlatformTransaction{
		Type:       PlatformTransactionPayout,
		ID:         d.UUID,
		OccurredAt: d.CreatedAt,
		Amount:     d.Amount,
		Payout:     d,
	}
}

// mergePlatformTransactions interleaves two lists, each already in statement order, into one
// list in the same order.
func mergePlatformTransactions(a, b []*PlatformTransaction, ascending bool) []*PlatformTransaction {
	merged := make([]*PlatformTransaction, 0, len(a)+len(b))

	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if platformLineBefore(a[i], b[j], ascending) {
			merged = append(merged, a[i])
			i++
		} else {
			merged = append(merged, b[j])
			j++
		}
	}
	merged = append(merged, a[i:]...)
	merged = append(merged, b[j:]...)

	return merged
}

// platformLineBefore reports whether x comes before y in statement order: by OccurredAt, then
// by ID compared byte-wise, as the repositories compare it under COLLATE "C".
func platformLineBefore(x, y *PlatformTransaction, ascending bool) bool {
	if !x.OccurredAt.Equal(y.OccurredAt) {
		if ascending {
			return x.OccurredAt.Before(y.OccurredAt)
		}
		return x.OccurredAt.After(y.OccurredAt)
	}
	if ascending {
		return x.ID < y.ID
	}
	return x.ID > y.ID
}

// encodePlatformCursor renders a statement position as an opaque token.
//
// The time goes in as UTC with nanosecond precision, which loses nothing: the columns are
// microsecond timestamps without a zone, read back as UTC, and the token is only ever
// compared against those same columns.
func encodePlatformCursor(cursor domain.KeysetCursor) string {
	raw := cursor.At.UTC().Format(time.RFC3339Nano) + "|" + cursor.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodePlatformCursor reads a token encodePlatformCursor issued. Empty is the first page and
// decodes to nil.
func decodePlatformCursor(token string) (*domain.KeysetCursor, error) {
	if token == "" {
		return nil, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("cursor is not base64url: %w", err)
	}

	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return nil, errors.New("cursor does not name a position")
	}

	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, fmt.Errorf("cursor time: %w", err)
	}

	return &domain.KeysetCursor{At: t, ID: id}, nil
}
