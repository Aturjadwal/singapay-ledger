package ledger

import (
	"context"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/repo"
)

// FakePlatformReconciliationRepository is the platform balance reconciliation's repository in
// memory. Its snapshot is computed from what the other fakes hold — the entries, the
// transactions, the payouts, its own adjustments — by the same rules as the Postgres
// statement, so the arithmetic under test is the ledger's, not a stub's.
type FakePlatformReconciliationRepository struct {
	fakes *FakeRepositoryProvider

	adjustments     []*domain.PlatformAdjustment
	reconciliations []*domain.PlatformReconciliation

	// beforeSnapshot runs before every ReadBalanceSnapshot with its 1-based call number: how a
	// test moves the ledger between the reads of one check.
	beforeSnapshot func(call int)
	snapshots      int
}

var _ domain.PlatformReconciliationRepository = (*FakePlatformReconciliationRepository)(nil)

func (f *FakeRepositoryProvider) PlatformReconciliation() domain.PlatformReconciliationRepository {
	return f.platformReconciliationRepo
}

// platformAmount is Σ of the platform's AVAILABLE PRODUCT_TRANSACTION entries for one
// transaction.
func (f *FakePlatformReconciliationRepository) platformAmount(platformAccountUUID, txUUID string) int64 {
	var sum int64
	for _, e := range f.fakes.ledgerEntryRepo.entries {
		if e.AccountUUID == platformAccountUUID && e.BalanceBucket == domain.BalanceBucketAvailable &&
			e.SourceType == domain.SourceTypeProductTransaction && e.SourceID == txUUID {
			sum += e.Amount
		}
	}
	return sum
}

func (f *FakePlatformReconciliationRepository) ReadBalanceSnapshot(ctx context.Context, platformAccountUUID string) (*domain.PlatformBalanceSnapshot, error) {
	f.snapshots++
	if f.beforeSnapshot != nil {
		f.beforeSnapshot(f.snapshots)
	}

	s := &domain.PlatformBalanceSnapshot{PlatformAccountUUID: platformAccountUUID}
	for _, e := range f.fakes.ledgerEntryRepo.entries {
		if e.AccountUUID == platformAccountUUID && e.BalanceBucket == domain.BalanceBucketAvailable {
			s.LedgerAvailable += e.Amount
		}
	}
	for _, tx := range f.fakes.productTransactionRepo.transactions {
		if tx.SellerAccountID == platformAccountUUID {
			continue
		}
		if tx.PlatformFeeTransferred {
			if tx.PlatformResidualMinor != nil {
				s.SweptResidualMinor += *tx.PlatformResidualMinor
			}
			continue
		}
		if tx.Status == domain.TransactionStatusSettled {
			amount := f.platformAmount(platformAccountUUID, tx.UUID)
			s.UnsweptPlatformFee += amount
			if amount != 0 {
				s.UnsweptCount++
			}
		}
	}
	for _, a := range f.adjustments {
		if a.PlatformAccountUUID != platformAccountUUID {
			continue
		}
		s.AdjustmentResidualMinor += a.ResidualMinor
		if a.Kind == domain.PlatformAdjustmentRounding {
			s.RoundedMinor += domain.RupiahToMinor(a.Amount)
		}
	}
	for _, d := range f.fakes.disbursementRepo.disbursements {
		if d.LedgerUUID == platformAccountUUID &&
			(d.Status == domain.DisbursementStatusPending || d.Status == domain.DisbursementStatusProcessing) {
			s.PayoutsInFlight++
		}
	}
	return s, nil
}

func (f *FakePlatformReconciliationRepository) rounded(txUUID string) bool {
	for _, a := range f.adjustments {
		if a.Kind != domain.PlatformAdjustmentRounding {
			continue
		}
		for _, id := range a.SourceTransactionUUIDs {
			if id == txUUID {
				return true
			}
		}
	}
	return false
}

func (f *FakePlatformReconciliationRepository) ListUnroundedResidualSources(ctx context.Context, platformAccountUUID string) ([]string, error) {
	var swept []*domain.ProductTransaction
	for _, tx := range f.fakes.productTransactionRepo.transactions {
		if tx.SellerAccountID == platformAccountUUID || !tx.PlatformFeeTransferred ||
			tx.PlatformResidualMinor == nil || *tx.PlatformResidualMinor <= 0 || f.rounded(tx.UUID) {
			continue
		}
		swept = append(swept, tx)
	}
	sort.Slice(swept, func(i, j int) bool {
		a, b := swept[i].PlatformFeeTransferredAt, swept[j].PlatformFeeTransferredAt
		if a != nil && b != nil && !a.Equal(*b) {
			return a.Before(*b)
		}
		return swept[i].UUID < swept[j].UUID
	})
	ids := []string{}
	for _, tx := range swept {
		ids = append(ids, tx.UUID)
	}
	return ids, nil
}

func (f *FakePlatformReconciliationRepository) ListReserveArchiveSources(ctx context.Context, platformAccountUUID string) ([]domain.ReserveArchiveSource, error) {
	sources := []domain.ReserveArchiveSource{}
	for _, tx := range f.fakes.productTransactionRepo.transactions {
		if tx.SettledPlatformFeeMinor != nil {
			continue
		}
		amount := f.platformAmount(platformAccountUUID, tx.UUID)
		if amount == 0 {
			continue
		}
		sources = append(sources, domain.ReserveArchiveSource{
			ProductTransactionUUID: tx.UUID,
			InvoiceNumber:          tx.InvoiceNumber,
			ProductType:            tx.ProductType,
			PlatformSale:           tx.SellerAccountID == platformAccountUUID,
			SettledAt:              tx.SettledAt,
			Amount:                 amount,
		})
	}
	sort.Slice(sources, func(i, j int) bool {
		a, b := sources[i].SettledAt, sources[j].SettledAt
		if a != nil && b != nil && !a.Equal(*b) {
			return a.Before(*b)
		}
		return sources[i].ProductTransactionUUID < sources[j].ProductTransactionUUID
	})
	return sources, nil
}

// SaveAdjustment mirrors the two unique indexes: one booking per check, one archive.
func (f *FakePlatformReconciliationRepository) SaveAdjustment(ctx context.Context, a *domain.PlatformAdjustment) error {
	for _, existing := range f.adjustments {
		if a.ReconciliationUUID != "" && existing.ReconciliationUUID == a.ReconciliationUUID {
			return fakeUniqueViolation(repo.ConstraintUniqueReconciliationBooking)
		}
		if a.Kind == domain.PlatformAdjustmentReserveArchive && existing.Kind == domain.PlatformAdjustmentReserveArchive &&
			existing.PlatformAccountUUID == a.PlatformAccountUUID {
			return fakeUniqueViolation(repo.ConstraintUniqueReserveArchive)
		}
	}
	stored := *a
	stored.SourceTransactionUUIDs = append([]string{}, a.SourceTransactionUUIDs...)
	f.adjustments = append(f.adjustments, &stored)
	return nil
}

func (f *FakePlatformReconciliationRepository) GetAdjustmentByReconciliation(ctx context.Context, reconciliationUUID string) (*domain.PlatformAdjustment, error) {
	for _, a := range f.adjustments {
		if a.ReconciliationUUID == reconciliationUUID {
			copied := *a
			return &copied, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakePlatformReconciliationRepository) GetReserveArchive(ctx context.Context) (*domain.PlatformAdjustment, error) {
	for _, a := range f.adjustments {
		if a.Kind == domain.PlatformAdjustmentReserveArchive {
			copied := *a
			return &copied, nil
		}
	}
	return nil, repo.ErrNotFound
}

func (f *FakePlatformReconciliationRepository) ListAdjustments(ctx context.Context, platformAccountUUID string, after *domain.KeysetCursor, limit int, ascending bool) ([]*domain.PlatformAdjustment, error) {
	var out []*domain.PlatformAdjustment
	for _, a := range f.adjustments {
		if a.PlatformAccountUUID == platformAccountUUID && keysetAfter(a.CreatedAt, a.UUID, after, ascending) {
			copied := *a
			out = append(out, &copied)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return keysetBefore(out[i].CreatedAt, out[i].UUID, out[j].CreatedAt, out[j].UUID, ascending)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *FakePlatformReconciliationRepository) SaveReconciliation(ctx context.Context, rec *domain.PlatformReconciliation) error {
	stored := *rec
	f.reconciliations = append(f.reconciliations, &stored)
	return nil
}

func (f *FakePlatformReconciliationRepository) LatestReconciliation(ctx context.Context, platformAccountUUID string) (*domain.PlatformReconciliation, error) {
	var latest *domain.PlatformReconciliation
	for _, rec := range f.reconciliations {
		if rec.PlatformAccountUUID != platformAccountUUID {
			continue
		}
		if latest == nil || rec.CheckedAt.After(latest.CheckedAt) ||
			(rec.CheckedAt.Equal(latest.CheckedAt) && !rec.CreatedAt.Before(latest.CreatedAt)) {
			latest = rec
		}
	}
	if latest == nil {
		return nil, repo.ErrNotFound
	}
	copied := *latest
	return &copied, nil
}

func (f *FakePlatformReconciliationRepository) LastEmailedAt(ctx context.Context, platformAccountUUID string) (*time.Time, error) {
	var last *time.Time
	for _, rec := range f.reconciliations {
		if rec.PlatformAccountUUID == platformAccountUUID && rec.EmailedAt != nil && (last == nil || rec.EmailedAt.After(*last)) {
			t := *rec.EmailedAt
			last = &t
		}
	}
	return last, nil
}

func (f *FakePlatformReconciliationRepository) MarkEmailed(ctx context.Context, reconciliationUUID string, at time.Time) error {
	for _, rec := range f.reconciliations {
		if rec.UUID == reconciliationUUID {
			t := at
			rec.EmailedAt = &t
			return nil
		}
	}
	return repo.ErrNotFound
}

// fakeUniqueViolation is the error a Postgres insert refused by the named unique index wraps.
func fakeUniqueViolation(constraint string) error {
	return repo.ErrFailedInsertSQL.WithError(&pq.Error{Code: "23505", Constraint: constraint})
}
