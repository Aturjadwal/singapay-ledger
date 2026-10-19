package repo

import (
	"context"
	"database/sql"
	"errors"
)

type TransactionProvider interface {
	Transact(ctx context.Context, fn func(tx Tx) error) error

	// TransactReadCommitted is Transact at READ COMMITTED, whatever the database's default:
	// each statement takes its own snapshot, so a read sees what other transactions
	// committed after this one began. (BEGIN ISOLATION LEVEL READ COMMITTED: no extra round
	// trip.)
	TransactReadCommitted(ctx context.Context, fn func(tx Tx) error) error
}

type PostgresTxProvider struct {
	db *sql.DB
}

func NewTransactionProvider(db *sql.DB) *PostgresTxProvider {
	return &PostgresTxProvider{db: db}
}

func (p *PostgresTxProvider) Transact(
	ctx context.Context,
	fn func(tx Tx) error,
) error {
	return p.transact(ctx, nil, fn)
}

func (p *PostgresTxProvider) TransactReadCommitted(ctx context.Context, fn func(tx Tx) error) error {
	return p.transact(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted}, fn)
}

func (p *PostgresTxProvider) transact(ctx context.Context, opts *sql.TxOptions, fn func(tx Tx) error) error {
	sqlTx, err := p.db.BeginTx(ctx, opts)
	if err != nil {
		return err
	}

	tx := &postgresTx{tx: sqlTx}

	if err := fn(tx); err != nil {
		if rbErr := sqlTx.Rollback(); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}

	return sqlTx.Commit()
}
