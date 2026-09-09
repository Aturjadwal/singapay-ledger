package repo

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/21strive/redifu"
	"github.com/Aturjadwal/singapay-ledger/domain"
)

type PostgresSettlementItemRepository struct {
	db DBTX
}

func NewPostgresSettlementItemRepository(db DBTX) *PostgresSettlementItemRepository {
	return &PostgresSettlementItemRepository{db: db}
}

func (r *PostgresSettlementItemRepository) GetByID(ctx context.Context, id string) (*domain.SettlementItem, error) {
	query := `
		SELECT uuid, randid, settlement_batch_uuid, product_transaction_uuid, seller_account_id,
		       invoice_number, gateway_account_id, gateway_transaction_id, payment_channel,
		       transaction_amount, pay_to_merchant, allocated_fee, fee_reported,
		       is_matched, expected_net_amount, amount_discrepancy,
		       raw_gateway_data, created_at, updated_at
		FROM settlement_items
		WHERE uuid = $1
	`

	row := r.db.QueryRowContext(ctx, query, id)
	return r.scanSettlementItem(row)
}

func (r *PostgresSettlementItemRepository) GetBySettlementBatchID(ctx context.Context, batchID string) ([]*domain.SettlementItem, error) {
	query := `
		SELECT uuid, randid, settlement_batch_uuid, product_transaction_uuid, seller_account_id,
		       invoice_number, gateway_account_id, gateway_transaction_id, payment_channel,
		       transaction_amount, pay_to_merchant, allocated_fee, fee_reported,
		       is_matched, expected_net_amount, amount_discrepancy,
		       raw_gateway_data, created_at, updated_at
		FROM settlement_items
		WHERE settlement_batch_uuid = $1
		ORDER BY created_at ASC
	`

	rows, err := r.db.QueryContext(ctx, query, batchID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanSettlementItems(rows)
}

func (r *PostgresSettlementItemRepository) GetByProductTransactionID(ctx context.Context, productTxID string) ([]*domain.SettlementItem, error) {
	query := `
		SELECT uuid, randid, settlement_batch_uuid, product_transaction_uuid, seller_account_id,
		       invoice_number, gateway_account_id, gateway_transaction_id, payment_channel,
		       transaction_amount, pay_to_merchant, allocated_fee, fee_reported,
		       is_matched, expected_net_amount, amount_discrepancy,
		       raw_gateway_data, created_at, updated_at
		FROM settlement_items
		WHERE product_transaction_uuid = $1
		ORDER BY created_at DESC
	`

	rows, err := r.db.QueryContext(ctx, query, productTxID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanSettlementItems(rows)
}

func (r *PostgresSettlementItemRepository) GetUnmatchedByBatchID(ctx context.Context, batchID string) ([]*domain.SettlementItem, error) {
	query := `
		SELECT uuid, randid, settlement_batch_uuid, product_transaction_uuid, seller_account_id,
		       invoice_number, gateway_account_id, gateway_transaction_id, payment_channel,
		       transaction_amount, pay_to_merchant, allocated_fee, fee_reported,
		       is_matched, expected_net_amount, amount_discrepancy,
		       raw_gateway_data, created_at, updated_at
		FROM settlement_items
		WHERE settlement_batch_uuid = $1 AND is_matched = false
		ORDER BY created_at ASC
	`

	rows, err := r.db.QueryContext(ctx, query, batchID)
	if err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}
	defer rows.Close()

	return r.scanSettlementItems(rows)
}

func (r *PostgresSettlementItemRepository) Save(ctx context.Context, item *domain.SettlementItem) error {
	rawGatewayDataJSON, err := json.Marshal(item.RawGatewayData)
	if err != nil {
		rawGatewayDataJSON = []byte("{}")
	}

	query := `
		INSERT INTO settlement_items (
			uuid, randid, settlement_batch_uuid, product_transaction_uuid, seller_account_id,
			invoice_number, gateway_account_id, gateway_transaction_id, payment_channel,
			transaction_amount, pay_to_merchant, allocated_fee, fee_reported,
			is_matched, expected_net_amount, amount_discrepancy,
			raw_gateway_data, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (uuid) DO UPDATE SET
			product_transaction_uuid = EXCLUDED.product_transaction_uuid,
			seller_account_id = EXCLUDED.seller_account_id,
			is_matched = EXCLUDED.is_matched,
			expected_net_amount = EXCLUDED.expected_net_amount,
			amount_discrepancy = EXCLUDED.amount_discrepancy,
			updated_at = EXCLUDED.updated_at
	`

	var productTxID *string
	if item.ProductTransactionUUID != "" {
		productTxID = &item.ProductTransactionUUID
	}

	var sellerAccountID *string
	if item.SellerAccountID != "" {
		sellerAccountID = &item.SellerAccountID
	}

	_, err = r.db.ExecContext(ctx, query,
		item.UUID,
		item.RandId,
		item.SettlementBatchUUID,
		productTxID,
		sellerAccountID,
		item.InvoiceNumber,
		item.GatewayAccountID,
		item.GatewayTransactionID,
		item.PaymentChannel,
		item.TransactionAmount,
		item.PayToMerchant,
		item.AllocatedFee,
		item.FeeReported,
		item.IsMatched,
		item.ExpectedNetAmount,
		item.AmountDiscrepancy,
		rawGatewayDataJSON,
		item.CreatedAt,
		item.UpdatedAt,
	)
	if err != nil {
		return ErrFailedInsertSQL.WithError(err)
	}

	return nil
}

func (r *PostgresSettlementItemRepository) SaveBatch(ctx context.Context, items []*domain.SettlementItem) error {
	for _, item := range items {
		if err := r.Save(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresSettlementItemRepository) scanSettlementItem(row *sql.Row) (*domain.SettlementItem, error) {
	var item domain.SettlementItem
	redifu.InitRecord(&item)
	var productTxID sql.NullString
	var sellerAccountID sql.NullString
	var invoiceNumber sql.NullString
	var gatewayAccountID sql.NullString
	var gatewayTransactionID sql.NullString
	var paymentChannel sql.NullString
	var rawGatewayDataJSON []byte

	err := row.Scan(
		&item.UUID,
		&item.RandId,
		&item.SettlementBatchUUID,
		&productTxID,
		&sellerAccountID,
		&invoiceNumber,
		&gatewayAccountID,
		&gatewayTransactionID,
		&paymentChannel,
		&item.TransactionAmount,
		&item.PayToMerchant,
		&item.AllocatedFee,
		&item.FeeReported,
		&item.IsMatched,
		&item.ExpectedNetAmount,
		&item.AmountDiscrepancy,
		&rawGatewayDataJSON,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, ErrFailedScanSQL.WithError(err)
	}

	if productTxID.Valid {
		item.ProductTransactionUUID = productTxID.String
	}
	if sellerAccountID.Valid {
		item.SellerAccountID = sellerAccountID.String
	}
	if invoiceNumber.Valid {
		item.InvoiceNumber = invoiceNumber.String
	}
	if gatewayAccountID.Valid {
		item.GatewayAccountID = gatewayAccountID.String
	}
	if gatewayTransactionID.Valid {
		item.GatewayTransactionID = gatewayTransactionID.String
	}
	if paymentChannel.Valid {
		item.PaymentChannel = paymentChannel.String
	}

	item.RawGatewayData = make(map[string]string)
	if len(rawGatewayDataJSON) > 0 {
		_ = json.Unmarshal(rawGatewayDataJSON, &item.RawGatewayData)
	}

	return &item, nil
}

func (r *PostgresSettlementItemRepository) scanSettlementItems(rows *sql.Rows) ([]*domain.SettlementItem, error) {
	var items []*domain.SettlementItem

	for rows.Next() {
		var item domain.SettlementItem
		redifu.InitRecord(&item)
		var productTxID sql.NullString
		var sellerAccountID sql.NullString
		var invoiceNumber sql.NullString
		var gatewayAccountID sql.NullString
		var gatewayTransactionID sql.NullString
		var paymentChannel sql.NullString
		var rawGatewayDataJSON []byte

		err := rows.Scan(
			&item.UUID,
			&item.RandId,
			&item.SettlementBatchUUID,
			&productTxID,
			&sellerAccountID,
			&invoiceNumber,
			&gatewayAccountID,
			&gatewayTransactionID,
			&paymentChannel,
			&item.TransactionAmount,
			&item.PayToMerchant,
			&item.AllocatedFee,
			&item.FeeReported,
			&item.IsMatched,
			&item.ExpectedNetAmount,
			&item.AmountDiscrepancy,
			&rawGatewayDataJSON,
			&item.CreatedAt,
			&item.UpdatedAt,
		)
		if err != nil {
			return nil, ErrFailedScanSQL.WithError(err)
		}

		if productTxID.Valid {
			item.ProductTransactionUUID = productTxID.String
		}
		if sellerAccountID.Valid {
			item.SellerAccountID = sellerAccountID.String
		}
		if invoiceNumber.Valid {
			item.InvoiceNumber = invoiceNumber.String
		}
		if gatewayAccountID.Valid {
			item.GatewayAccountID = gatewayAccountID.String
		}
		if gatewayTransactionID.Valid {
			item.GatewayTransactionID = gatewayTransactionID.String
		}
		if paymentChannel.Valid {
			item.PaymentChannel = paymentChannel.String
		}

		item.RawGatewayData = make(map[string]string)
		if len(rawGatewayDataJSON) > 0 {
			_ = json.Unmarshal(rawGatewayDataJSON, &item.RawGatewayData)
		}

		items = append(items, &item)
	}

	if err := rows.Err(); err != nil {
		return nil, ErrFailedQuerySQL.WithError(err)
	}

	return items, nil
}
