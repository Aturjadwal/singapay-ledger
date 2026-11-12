package repo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
)

// PostgresPlatformFeeReconciliationRepository implements
// domain.PlatformFeeReconciliationRepository: the card reconciliation's reads, and its two
// insert-only tables (migration 032).
type PostgresPlatformFeeReconciliationRepository struct {
	db DBTX
}

func NewPostgresPlatformFeeReconciliationRepository(db DBTX) *PostgresPlatformFeeReconciliationRepository {
	return &PostgresPlatformFeeReconciliationRepository{db: db}
}

// estimatedCardFeeSQL selects the card reconciliation's candidates, over product_transactions
// as pt: settled, not transferred, fee figures still the checkout estimate, a card payment
// whose gateway fee the payer paid, with a platform fee to sweep.
//
// The first three conditions are migration 033's partial index predicate, written as the
// index has it, so the planner can prove the index serves the query. A transaction has one
// payment request; EXISTS keeps a duplicate from repeating the transaction even so.
const estimatedCardFeeSQL = `pt.status = 'SETTLED'
	  AND pt.platform_fee_transferred = false
	  AND pt.settled_fee_source = 'ESTIMATE'
	  AND pt.fee_model = 'GATEWAY_ON_CUSTOMER'
	  AND pt.platform_fee > 0
	  AND EXISTS (SELECT 1 FROM payment_requests pr
	               WHERE pr.product_transaction_uuid = pt.uuid
	                 AND pr.payment_channel = 'CREDIT_CARD')`

// SellersWithEstimatedCardFees returns the seller accounts holding card candidates, the one
// whose oldest candidate settled first leading. Only SELLER accounts: the balance equation
// is a seller's, and means nothing for the platform's own sub-account.
func (r *PostgresPlatformFeeReconciliationRepository) SellersWithEstimatedCardFees(ctx context.Context, limit int) ([]string, error) {
	query := fmt.Sprintf(`
		SELECT pt.seller_account_id
		FROM product_transactions pt
		JOIN ledger_accounts a ON a.uuid = pt.seller_account_id
		WHERE a.owner_type = 'SELLER'
		  AND %s
		GROUP BY pt.seller_account_id
		ORDER BY MIN(pt.settled_at) ASC, pt.seller_account_id ASC
		LIMIT $1
	`, estimatedCardFeeSQL)

	rows, err := r.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	var sellers []string
	for rows.Next() {
		var seller string
		if err := rows.Scan(&seller); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
		sellers = append(sellers, seller)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return sellers, nil
}

// ReadCardFeeSnapshot reads one seller's side of the balance equation in a single
// statement, so every figure is from the same snapshot.
//
//   - seller_available: the sum of the seller's AVAILABLE entries, whole rupiah, as a
//     withdrawal derives it (GetAllBalances), not the cached column.
//   - certain_unswept: platform fee that is final but not transferred, in sen. NULL
//     settled_fee_source is GATEWAY here as in the sweep, and BALANCE and MANUAL are final.
//   - in_flight: the seller's payouts in PENDING or PROCESSING.
//   - one row per candidate card, oldest settlement first; a single row of NULLs when there
//     is none, which still carries the three figures.
//
// The three scalar subqueries do not depend on the candidate, so PostgreSQL evaluates each
// once for the statement.
func (r *PostgresPlatformFeeReconciliationRepository) ReadCardFeeSnapshot(ctx context.Context, sellerAccountID string) (*domain.CardFeeSnapshot, error) {
	query := fmt.Sprintf(`
		SELECT s.seller_available, s.certain_unswept, s.in_flight,
		       c.uuid, c.invoice_number, c.platform_fee, c.gateway_fee, c.total_charged, c.seller_net_amount,
		       c.settled_platform_fee, c.settled_gateway_fee, c.platform_residual, c.settled_at
		FROM (
			SELECT
				(SELECT COALESCE(SUM(le.amount), 0)::bigint
				   FROM ledger_entries le
				  WHERE le.account_uuid = $1
				    AND le.balance_bucket = 'AVAILABLE') AS seller_available,
				(SELECT COALESCE(SUM(COALESCE(u.settled_platform_fee, u.platform_fee * 100)), 0)::bigint
				   FROM product_transactions u
				  WHERE u.seller_account_id = $1
				    AND u.status = 'SETTLED'
				    AND u.platform_fee_transferred = false
				    AND COALESCE(u.settled_fee_source, 'GATEWAY') <> 'ESTIMATE') AS certain_unswept,
				(SELECT COUNT(*)
				   FROM disbursements d
				  WHERE d.account_uuid = $1
				    AND d.status IN ('PENDING', 'PROCESSING')) AS in_flight
		) s
		LEFT JOIN LATERAL (
			SELECT pt.uuid, pt.invoice_number, pt.platform_fee, pt.gateway_fee, pt.total_charged, pt.seller_net_amount,
			       pt.settled_platform_fee, pt.settled_gateway_fee, pt.platform_residual, pt.settled_at
			FROM product_transactions pt
			WHERE pt.seller_account_id = $1
			  AND %s
			  AND EXISTS (SELECT 1 FROM ledger_accounts a
			               WHERE a.uuid = pt.seller_account_id
			                 AND a.owner_type = 'SELLER')
		) c ON true
		ORDER BY c.settled_at ASC, c.uuid ASC
	`, estimatedCardFeeSQL)

	rows, err := r.db.QueryContext(ctx, query, sellerAccountID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	snapshot := &domain.CardFeeSnapshot{SellerAccountID: sellerAccountID, Cards: []domain.CardFeeCandidate{}}
	read := false
	for rows.Next() {
		var (
			card struct {
				UUID                    sql.NullString
				InvoiceNumber           sql.NullString
				PlatformFee             sql.NullInt64
				GatewayFee              sql.NullInt64
				TotalCharged            sql.NullInt64
				SellerNetAmount         sql.NullInt64
				SettledPlatformFeeMinor sql.NullInt64
				SettledGatewayFeeMinor  sql.NullInt64
				PlatformResidualMinor   sql.NullInt64
				SettledAt               sql.NullTime
			}
			inFlight int64
		)
		if err := rows.Scan(
			&snapshot.SellerLedgerAvailable, &snapshot.CertainUnsweptPlatformFeeMinor, &inFlight,
			&card.UUID, &card.InvoiceNumber, &card.PlatformFee, &card.GatewayFee, &card.TotalCharged, &card.SellerNetAmount,
			&card.SettledPlatformFeeMinor, &card.SettledGatewayFeeMinor, &card.PlatformResidualMinor, &card.SettledAt,
		); err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}
		read = true
		snapshot.DisbursementsInFlight = int(inFlight)

		if !card.UUID.Valid {
			continue // the row that says there is no candidate
		}
		candidate := domain.CardFeeCandidate{
			ProductTransactionUUID:  card.UUID.String,
			InvoiceNumber:           card.InvoiceNumber.String,
			PlatformFee:             card.PlatformFee.Int64,
			GatewayFee:              card.GatewayFee.Int64,
			TotalCharged:            card.TotalCharged.Int64,
			SellerNetAmount:         card.SellerNetAmount.Int64,
			SettledPlatformFeeMinor: nullableInt64(card.SettledPlatformFeeMinor),
			SettledGatewayFeeMinor:  nullableInt64(card.SettledGatewayFeeMinor),
			PlatformResidualMinor:   nullableInt64(card.PlatformResidualMinor),
		}
		if card.SettledAt.Valid {
			settledAt := card.SettledAt.Time
			candidate.SettledAt = &settledAt
		}
		snapshot.Cards = append(snapshot.Cards, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	if !read {
		// The statement always returns at least one row; none means it did not run as written.
		return nil, ErrFailedQuerySQL.WithError(fmt.Errorf("card fee snapshot for %s returned no row", sellerAccountID))
	}

	return snapshot, nil
}

// Save inserts one round's row.
func (r *PostgresPlatformFeeReconciliationRepository) Save(ctx context.Context, rec *domain.PlatformFeeReconciliation) error {
	query := `
		INSERT INTO platform_fee_reconciliations (
			uuid, randid, seller_account_id, singapay_account_id,
			singapay_available, singapay_pending, singapay_held,
			seller_ledger_available, certain_unswept_platform_fee, computed_platform_fee,
			nominal_platform_fee, tolerance, card_count, outcome, hold_reason, raw_balance, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	`

	var rawBalance sql.NullString
	if len(rec.RawBalance) > 0 {
		rawBalance = sql.NullString{String: string(rec.RawBalance), Valid: true}
	}

	_, err := r.db.ExecContext(ctx, query,
		rec.UUID,
		rec.RandId,
		rec.SellerAccountID,
		rec.SingapayAccountID,
		toNullInt64(rec.SingapayAvailableMinor),
		toNullInt64(rec.SingapayPendingMinor),
		toNullInt64(rec.SingapayHeldMinor),
		rec.SellerLedgerAvailableMinor,
		rec.CertainUnsweptPlatformFeeMinor,
		toNullInt64(rec.ComputedPlatformFeeMinor),
		rec.NominalPlatformFeeMinor,
		rec.ToleranceMinor,
		rec.CardCount,
		string(rec.Outcome),
		toNullString(string(rec.HoldReason)),
		rawBalance,
		createdAtOrNow(rec.CreatedAt),
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}
	return nil
}

// SaveItems inserts a round's cards, one statement each, inside the caller's transaction.
func (r *PostgresPlatformFeeReconciliationRepository) SaveItems(ctx context.Context, items []*domain.PlatformFeeReconciliationItem) error {
	query := `
		INSERT INTO platform_fee_reconciliation_items (
			uuid, randid, reconciliation_uuid, product_transaction_uuid,
			priced_platform_fee, previous_settled_gateway_fee, previous_settled_platform_fee,
			allocated_platform_fee, settled_gateway_fee, platform_residual,
			journal_uuid, ledger_entry_uuid, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`

	for _, item := range items {
		_, err := r.db.ExecContext(ctx, query,
			item.UUID,
			item.RandId,
			item.ReconciliationUUID,
			item.ProductTransactionUUID,
			item.PricedPlatformFeeMinor,
			item.PreviousSettledGatewayFeeMinor,
			item.PreviousSettledPlatformFeeMinor,
			toNullInt64(item.AllocatedPlatformFeeMinor),
			toNullInt64(item.SettledGatewayFeeMinor),
			toNullInt64(item.PlatformResidualMinor),
			toNullString(item.JournalUUID),
			toNullString(item.LedgerEntryUUID),
			createdAtOrNow(item.CreatedAt),
		)
		if err != nil {
			return ErrFailedInsertSQL.WithError(err)
		}
	}
	return nil
}

func nullableInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// createdAtOrNow guards a NOT NULL timestamp against an entity built without InitRecord.
func createdAtOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
