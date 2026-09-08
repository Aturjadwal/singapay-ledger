package ledger

import (
	"context"
	"time"

	"github.com/21strive/ledger/domain"
	"github.com/21strive/ledger/ledgererr"
)

// ─────────────────────────────────────────────────────────────────────────────
// Reconciliation — NOT IMPLEMENTED against Singapay. Read this before wiring it.
// ─────────────────────────────────────────────────────────────────────────────
//
// Reconciliation is the step that moves a seller's balance from PENDING to AVAILABLE.
// Until it runs, money can arrive and be booked, and payouts can be made against whatever
// is already AVAILABLE — but nothing new ever becomes withdrawable. That is the current
// state of this package, deliberately, and it is the one gap that stops the Singapay
// migration being complete.
//
// # Why it is not implemented rather than approximated
//
// Ledger entries are insert-only. A settlement booked on a wrong assumption cannot be
// undone, only compensated with a second set of entries and an audit — so an approximation
// here is more expensive than an absence. The absence is loud; a wrong PENDING→AVAILABLE
// conversion is silent, and it decides what a seller is allowed to withdraw.
//
// # What is settled, and what is not
//
// The mechanism is clear enough. Singapay announces a batch on settlement_notif_url with
// totals, a settlement_method, and a date window — and no list of the transactions it
// covered. The rows are reconstructed by replaying that window against the per-product
// transaction lists (Client.ListVATransactions and its three siblings) filtered on
// SettlementWindow. Virtual account, QRIS and e-wallet each report the fee Singapay
// actually took; a payment link reports none, anywhere.
//
// Four questions have to be answered against a live sandbox before that mechanism can be
// trusted to write entries. Each one has a wrong answer that produces a plausible-looking
// result:
//
//  1. Timezone and boundary of the settlement window. Singapay writes settlement
//     timestamps as text with no offset — "26 Dec 2025 13:35:45" — and Asia/Jakarta is an
//     assumption, not a documented fact. Seven hours in either direction moves rows
//     between batches: some settle twice, others never.
//
//  2. Which settlement event the window keys on. A QRIS transaction carries both
//     SettleAt and SettledToMerchantAt, and they are different moments. Filtering on the
//     wrong one silently returns the wrong set of rows.
//
//  3. Whether a batch's rows can be enumerated at acceptable cost. The reconstruction is
//     N accounts × up to 4 product lists × pagination, per settlement. Rate limits and
//     page behaviour are unmeasured. This one is a scaling question, not a correctness
//     one, but it decides whether the design is viable at all.
//
//  4. What settlement.refunded should do. It can pull back funds that have already become
//     AVAILABLE and may already have been withdrawn. This ledger has no negative-balance
//     policy, and inventing one inside a reconciliation routine is the wrong place to
//     decide it. This is a business decision, not a coding gap.
//
// cmd/singapay-smoke is where (1) and (2) get answered. (3) needs a sandbox with enough
// volume to page. (4) needs a person.
//
// # What already exists
//
// domain.SettlementBatch, domain.SettlementItem and domain.SettledTransaction are written
// for the Singapay shape, and the PaymentGateway interface carries the four list methods
// the reconstruction needs. The ledger-entry side — PENDING→AVAILABLE conversion, fee
// adjustment write-offs and credits, gateway fee clearance — is unchanged from a model
// that survives the migration intact, because Singapay's pending/available split matches
// this ledger's. What is missing is only the part that decides which rows belong to a
// batch, and that is exactly the part that is unverified.

// ReconciliationRequest describes one settlement batch to reconcile.
//
// The fields are shaped for Singapay's settlement webhook rather than a file upload:
// SettlementID is the idempotency key, and the window is what makes the covered rows
// findable at all. See the package note above for why nothing consumes this yet.
type ReconciliationRequest struct {
	// SettlementID is Singapay's own id for the settlement. It is the idempotency key:
	// a batch already booked under this id must never be booked again.
	SettlementID string
	// SettlementReference is Singapay's reference_no, the label a support conversation
	// will quote.
	SettlementReference string
	// SettleFrom and SettleTo bound the window the batch covered. Without them the rows
	// cannot be found: Singapay sends no list of them.
	SettleFrom time.Time
	SettleTo   time.Time
	// SettlementDate is the batch's nominal date, for reporting.
	SettlementDate time.Time
	// InitiatedBy names who caused this run — the webhook, or the operator replaying it.
	InitiatedBy string
}

// ReconciliationResponse reports what a reconciliation did.
type ReconciliationResponse struct {
	ReconciliationID string `json:"reconciliation_id"`
	// AlreadyIngested reports that this settlement was already booked and nothing was
	// posted. It is a success, not an error: a redelivered settlement webhook is
	// ordinary traffic, and the answer the caller needs is "already done".
	AlreadyIngested bool `json:"already_ingested,omitempty"`
	// IngestedAs is the settlement reference the batch was originally booked under, set
	// only when AlreadyIngested is true.
	IngestedAs     string                  `json:"ingested_as,omitempty"`
	InitiatedBy    string                  `json:"initiated_by"`
	InitiatedAt    time.Time               `json:"initiated_at"`
	SettlementDate string                  `json:"settlement_date"`
	Transactions   ReconciliationTxSummary `json:"transactions"`
	BalanceUpdates ReconciliationBalances  `json:"balance_updates"`
	Discrepancies  []DiscrepancySummary    `json:"discrepancies"`
	Verification   ReconciliationVerify    `json:"verification"`
}

// ReconciliationTxSummary contains transaction counts
type ReconciliationTxSummary struct {
	Total     int `json:"total"`
	Matched   int `json:"matched"`
	Unmatched int `json:"unmatched"`
}

// ReconciliationBalances contains balance changes
type ReconciliationBalances struct {
	Pending   BalanceChange `json:"pending"`
	Available BalanceChange `json:"available"`
}

// BalanceChange represents before/after/diff for a balance
type BalanceChange struct {
	Before int64 `json:"before"`
	After  int64 `json:"after"`
	Diff   int64 `json:"diff"`
}

// DiscrepancySummary contains discrepancy information
type DiscrepancySummary struct {
	Type          string `json:"type"`
	InvoiceNumber string `json:"invoice_number,omitempty"`
	Amount        int64  `json:"amount,omitempty"`
	Message       string `json:"message"`
}

// ReconciliationVerify reports how our derived balances compared with the gateway's.
type ReconciliationVerify struct {
	GatewayAPIChecked  bool   `json:"gateway_api_checked"`
	MatchStatus        string `json:"match_status"`
	SellersVerified    int    `json:"sellers_verified"`
	SellersMatched     int    `json:"sellers_matched"`
	SellersMismatched  int    `json:"sellers_mismatched"`
	SellersNotVerified int    `json:"sellers_not_verified"`
}

// ProcessReconciliation is not implemented against Singapay and refuses to run.
//
// It returns ErrReconciliationNotImplemented rather than being absent so that a caller
// wiring the settlement webhook gets a clear, immediate answer instead of a compile error
// they route around, or — far worse — a silent no-op that looks like a settlement with
// nothing in it. Read the note at the top of this file for the four questions that must be
// answered before this can write ledger entries.
func (c *LedgerClient) ProcessReconciliation(ctx context.Context, req *ReconciliationRequest) (*ReconciliationResponse, error) {
	c.logger.ErrorContext(ctx, "ProcessReconciliation was called but is not implemented for Singapay — no balances were moved",
		"settlement_id", req.SettlementID,
		"settlement_reference", req.SettlementReference,
		"settle_from", req.SettleFrom,
		"settle_to", req.SettleTo,
	)
	return nil, ErrReconciliationNotImplemented
}

// ErrReconciliationNotImplemented is returned by ProcessReconciliation. Callers should
// treat it as a configuration state, not a transient failure: retrying will not help, and
// the settlement webhook that triggered it should be acknowledged rather than retried by
// Singapay.
var ErrReconciliationNotImplemented = ledgererr.NewError(
	ledgererr.CodeReconciliationNotImplemented,
	"settlement reconciliation is not implemented for Singapay; PENDING balances will not become AVAILABLE until it is",
	nil,
)

// FilterIngestedSettlements returns the subset of settlementIDs this ledger has already
// booked as settlement batches.
//
// It exists for a caller replaying a range of settlements — after an outage that dropped
// webhooks, say: list the settlements, ask here which are already booked, process the
// difference. Asking the ledger is the point, because "have I booked this settlement?" is
// a question about ledger state, and answering it with SQL from outside this package
// couples that caller to a schema it does not own.
//
// It answers correctly today even though ProcessReconciliation does not run — the table is
// simply empty of Singapay batches, so everything reads as unprocessed.
func (c *LedgerClient) FilterIngestedSettlements(ctx context.Context, settlementIDs []string) (map[string]struct{}, error) {
	ingested, err := c.repoProvider.SettlementBatch().FilterIngestedBatchIDs(ctx, settlementIDs)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to filter ingested settlements", err)
	}

	return ingested, nil
}

// GetSettlementBatch returns one booked settlement batch by its internal id.
func (c *LedgerClient) GetSettlementBatch(ctx context.Context, id string) (*domain.SettlementBatch, error) {
	batch, err := c.repoProvider.SettlementBatch().GetByID(ctx, id)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to get settlement batch", err)
	}
	return batch, nil
}
