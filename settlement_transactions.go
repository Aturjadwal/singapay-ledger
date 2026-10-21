package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// ─────────────────────────────────────────────────────────────────────────────
// Settlement — what one batch covered, read back from Singapay.
// ─────────────────────────────────────────────────────────────────────────────
//
// A settlement webhook names a batch and its totals and nothing it covered, and the
// settling pass never needs more (settlement.go). A person reading the settlement inbox
// does: which payments were in SETTLEMENT-647-…? Singapay answers that itself — its
// settlement-transactions listing gives a completed batch's contents — and this puts each
// row it lists beside the ledger's own record of the same payment.
//
// Read only, and it decides nothing. A listed payment the ledger still holds as COMPLETED
// is reported as such, not settled from here: the pass, or CheckTransactionSettlement
// behind its own button, does that through the same compare-and-set as always. Nor does a
// listing feed the pass — its contents are Singapay's answer about a batch, and the pass
// asks about invoices, for the reasons settlement.go gives.

// Page sizes for ListSettlementTransactions. Singapay documents no maximum; these keep one
// page to what a person reads at once, and one gateway call bounded.
const (
	DefaultSettlementTransactionsPerPage = 50
	MaxSettlementTransactionsPerPage     = 100
)

// SettlementTransactionMatch says how a row Singapay listed was tied to a ledger
// transaction.
type SettlementTransactionMatch string

const (
	// SettlementMatchInvoice: the row's merchant_reff_no — or failing that its transaction_id
	// — is a ledger invoice number, the reference this ledger issues payments under.
	SettlementMatchInvoice SettlementTransactionMatch = "INVOICE_NUMBER"
	// SettlementMatchGatewayID: the row's transaction_id is a gateway identifier the ledger
	// stored for the payment (gateway_transaction_ref or gateway_transaction_id) — how a
	// payment link's attempt is found, a link being created with the invoice as reff_no
	// rather than merchant_reff_no.
	SettlementMatchGatewayID SettlementTransactionMatch = "GATEWAY_ID"
	// SettlementMatchNone: nothing in the ledger matches the row.
	SettlementMatchNone SettlementTransactionMatch = "NONE"
)

// SettlementTransactionsPage is one page of what a settlement covered.
type SettlementTransactionsPage struct {
	// Notification is the stored webhook the settlement was read through.
	Notification *domain.SettlementNotification

	Transactions []SettlementTransaction

	// Page and PerPage are the page Singapay answered with: what it reported, or what was
	// asked for when it reported nothing.
	Page    int
	PerPage int
	// Total and TotalPages are Singapay's counts across every page; 0 when it sent none.
	Total      int
	TotalPages int
	// HasMore says whether a later page exists — from Singapay's counts when it sent them,
	// otherwise from whether this page came back full.
	HasMore bool
}

// SettlementTransaction is one payment a settlement covered: the row Singapay listed, and
// the ledger's record of the same payment when one matches.
type SettlementTransaction struct {
	Gateway SettlementGatewayTransaction

	Match SettlementTransactionMatch
	// Transaction is the matched payment; nil when Match is SettlementMatchNone.
	Transaction *domain.ProductTransaction
	// Account is the ledger account the payment belongs to: the matched transaction's
	// seller, else the account whose Singapay sub-account the row names. nil when the
	// ledger knows neither. A matched transaction whose seller's sub-account is not the one
	// Singapay names is worth a look: Account.SingapayAccountID and
	// Gateway.AccountID then differ.
	Account *domain.Account
	// SubAccount is the account holding the Singapay sub-account the row was paid into —
	// whose money it became, whatever payment it matched. nil when the row names no
	// sub-account or the ledger holds none by that id.
	SubAccount *domain.Account
}

// SettlementGatewayTransaction is a row of Singapay's settlement listing, in the ledger's
// units: every amount in sen, every absent timestamp nil.
type SettlementGatewayTransaction struct {
	// TransactionID is Singapay's business id: a VA payment's transaction_id, every other
	// product's reff_no.
	TransactionID string
	// MerchantReference is merchant_reff_no, the reference the payment was created with.
	MerchantReference string
	// AccountID is the Singapay sub-account (ULID) the payment was made to; AccountName is
	// Singapay's name for it.
	AccountID   string
	AccountName string
	// PaymentMethod is Singapay's name for how it was paid ("BRI Virtual Account").
	PaymentMethod string

	// AmountMinor is what the payment was for. FeeMinor is the fee Singapay charged the
	// merchant, nil when it reported none — which is not a zero fee.
	AmountMinor int64
	FeeMinor    *int64
	Currency    string

	PostedAt *time.Time
	// PaidAt is processed_timestamp; Singapay reports none for QRIS.
	PaidAt *time.Time
	// Settled is has_settle and SettledAt settle_at.
	Settled   bool
	SettledAt *time.Time
}

// ListSettlementTransactions reads one page of the payments a settlement covered, as
// Singapay lists them, each beside the ledger's record of the same payment.
//
// notificationID is a stored settlement webhook's UUID; the settlement is the one it names,
// whatever its event — a refund event names the batch the refunded payment was in. page
// starts at 1; perPage defaults to DefaultSettlementTransactionsPerPage and is capped at
// MaxSettlementTransactionsPerPage.
//
// A row is matched by its merchant_reff_no against invoice numbers first, then by its
// transaction_id against the gateway identifiers the ledger stored. A row neither finds is
// returned unmatched rather than dropped: a payment Singapay settled that the ledger cannot
// place is exactly what someone reading a settlement needs to see.
//
// Errors: ErrSettlementNotificationNotFound for an unknown notification;
// ErrSettlementNotListed when Singapay answers 404 (the batch is not completed, or not this
// merchant's); CodeGatewayAPIError when Singapay could not be read otherwise; and
// CodeDatabaseError. Nothing is written in any case.
func (c *LedgerClient) ListSettlementTransactions(ctx context.Context, notificationID string, page, perPage int) (*SettlementTransactionsPage, error) {
	notification, err := c.settlementNotification(ctx, notificationID)
	if err != nil {
		return nil, err
	}
	page = max(page, 1)
	if perPage <= 0 {
		perPage = DefaultSettlementTransactionsPerPage
	}
	perPage = min(perPage, MaxSettlementTransactionsPerPage)

	answered, err := c.readSettlementPage(ctx, notification, page, perPage)
	if err != nil {
		return nil, err
	}
	listed, pagination := answered.rows, answered.pagination

	result := &SettlementTransactionsPage{
		Notification: notification,
		Page:         cmp.Or(pagination.CurrentPage, page),
		PerPage:      cmp.Or(pagination.PerPage, perPage),
		Total:        pagination.Total,
		TotalPages:   pagination.TotalPages,
	}
	switch {
	case result.Page != page:
		// Singapay answered another page than the one asked for: it does not honour `page`,
		// which its documentation does not list. Those rows are some other page's, and showing
		// them again would repeat rows, so this page is empty and the listing ends here.
		c.logger.WarnContext(ctx, "Singapay answered another page of a settlement's transactions than the one asked for",
			"settlement_notification_uuid", notification.UUID,
			"settlement_id", notification.SettlementID,
			"asked_page", page,
			"answered_page", result.Page,
		)
		listed = nil
		result.Page = page
		result.HasMore = false
	case pagination.TotalPages > 0:
		result.HasMore = result.Page < pagination.TotalPages
	case pagination.Total > 0:
		result.HasMore = result.Page*result.PerPage < pagination.Total
	default:
		result.HasMore = len(listed) >= result.PerPage
	}
	result.Transactions = make([]SettlementTransaction, len(listed))

	for i, row := range listed {
		result.Transactions[i] = SettlementTransaction{
			Gateway: settlementGatewayTransaction(row),
			Match:   SettlementMatchNone,
		}
	}
	if err := c.matchSettlementTransactions(ctx, result.Transactions); err != nil {
		return nil, err
	}
	if err := c.attachSettlementAccounts(ctx, result.Transactions); err != nil {
		return nil, err
	}

	matched := 0
	for _, t := range result.Transactions {
		if t.Match != SettlementMatchNone {
			matched++
		}
	}
	c.logger.InfoContext(ctx, "Listed a settlement's transactions",
		"settlement_notification_uuid", notification.UUID,
		"settlement_id", notification.SettlementID,
		"settlement_reference", notification.SettlementReference,
		"page", result.Page,
		"rows", len(result.Transactions),
		"matched", matched,
		"total", result.Total,
	)

	return result, nil
}

// SettlementListing is every row Singapay lists for one settlement, read page by page and
// not yet put beside the ledger.
//
// It is Singapay's answer about a completed batch, which does not change, so a consumer may
// keep it and put it beside the ledger again (MatchSettlementTransactions) each time it is
// shown: the ledger's side is what moves, as the daily pass settles what the batch covered.
type SettlementListing struct {
	// Notification is the stored webhook the settlement was read through.
	Notification *domain.SettlementNotification
	// Rows are in Singapay's order, page after page.
	Rows []SettlementGatewayTransaction
	// Total is Singapay's count across every page, as its first page reported it; 0 when it
	// sent none.
	Total int
	// RepeatedRows counts the rows whose transaction id an earlier row already had. One
	// payment is one row of a batch, so a repeat is Singapay's order shifting between the
	// pages as they were read (a row shown twice, another not at all) — or a payment settled
	// twice. The rows are kept as listed.
	RepeatedRows int
	// Complete says the rows are the whole batch: every page read, as many rows as Singapay
	// counts (when it counts), and no transaction id twice. It is false when the row cap
	// ended the read, when a page came back as another page than the one asked for (only the
	// rows before it are kept, and nothing after it is read), when the rows and Singapay's
	// count disagree, and when RepeatedRows is not 0.
	Complete bool
}

// MaxSettlementListingRows bounds ReadSettlementListing. A batch is the merchant's whole
// window — every seller's payments — so it grows with the platform; the largest so far
// held 7. Ten thousand rows is a hundred pages, read four at a time within one request's
// deadline.
const MaxSettlementListingRows = 10_000

// settlementListingConcurrency is how many pages ReadSettlementListing asks Singapay for at
// once, after the first page has said how many there are.
const settlementListingConcurrency = 4

// ReadSettlementListing reads every row Singapay lists for the settlement a stored
// notification names, MaxSettlementTransactionsPerPage a page, at most maxRows rows (0, or
// anything above MaxSettlementListingRows, is MaxSettlementListingRows). Nothing is matched
// to the ledger: MatchSettlementTransactions does that, as often as the rows are shown.
//
// Singapay's documentation lists no `page` parameter, so each page is checked as it comes
// back: one answered as another page number, or opening on a row an earlier page already
// listed (Singapay serving page 1 again without saying so), ends the listing there. When
// the first page counts the pages (or the rows), page 2 is read alone and checked before
// the rest are read settlementListingConcurrency at a time — a Singapay that ignores `page`
// costs two calls, not a hundred; when it counts nothing, the pages are read one after
// another until one comes back short. See SettlementListing.Complete for when a listing is
// whole.
//
// Errors are ListSettlementTransactions's: one page Singapay fails to answer fails the read,
// so a listing is never silently missing a page in the middle.
func (c *LedgerClient) ReadSettlementListing(ctx context.Context, notificationID string, maxRows int) (*SettlementListing, error) {
	notification, err := c.settlementNotification(ctx, notificationID)
	if err != nil {
		return nil, err
	}
	if maxRows <= 0 || maxRows > MaxSettlementListingRows {
		maxRows = MaxSettlementListingRows
	}
	started := time.Now()

	const perPage = MaxSettlementTransactionsPerPage
	first, err := c.readSettlementPage(ctx, notification, 1, perPage)
	if err != nil {
		return nil, err
	}
	// Singapay may answer a smaller page than asked for; its page numbers count its pages.
	pageSize := first.pagination.PerPage
	if pageSize <= 0 {
		pageSize = perPage
	}
	maxPages := (maxRows + pageSize - 1) / pageSize
	pageCount := 0
	switch {
	case first.pagination.TotalPages > 0:
		pageCount = first.pagination.TotalPages
	case first.pagination.Total > 0:
		pageCount = (first.pagination.Total + pageSize - 1) / pageSize
	}

	listing := &SettlementListing{Notification: notification, Total: max(first.pagination.Total, 0)}
	seen := map[string]bool{}
	pagesRead := 1
	// accept adds a page's rows, or refuses a page that is not the one asked for.
	accept := func(asked int, page settlementPage) bool {
		answered := cmp.Or(page.pagination.CurrentPage, asked)
		repeatsAPage := asked > 1 && len(page.rows) > 0 && page.rows[0].TransactionID != "" && seen[page.rows[0].TransactionID]
		if answered != asked || repeatsAPage {
			c.logger.WarnContext(ctx, "Singapay answered another page of a settlement's transactions than the one asked for",
				"settlement_notification_uuid", notification.UUID,
				"settlement_id", notification.SettlementID,
				"asked_page", asked,
				"answered_page", answered,
				"opens_on_a_listed_row", repeatsAPage,
			)
			return false
		}
		for _, row := range page.rows {
			listed := settlementGatewayTransaction(row)
			if listed.TransactionID != "" {
				if seen[listed.TransactionID] {
					listing.RepeatedRows++
				}
				seen[listed.TransactionID] = true
			}
			listing.Rows = append(listing.Rows, listed)
		}
		return true
	}

	complete := false
	switch {
	case !accept(1, first):
		// Even page 1 came back as another page: nothing is kept.
	case pageCount > 1 && maxPages > 1:
		pagesRead++
		second, err := c.readSettlementPage(ctx, notification, 2, perPage)
		if err != nil {
			return nil, err
		}
		if !accept(2, second) {
			break
		}
		rest, err := c.readSettlementPages(ctx, notification, 3, min(pageCount, maxPages), perPage)
		if err != nil {
			return nil, err
		}
		pagesRead += len(rest)
		complete = pageCount <= maxPages
		for i, page := range rest {
			if !accept(3+i, page) {
				complete = false
				break
			}
		}
	case pageCount > 0:
		complete = pageCount <= maxPages
	default:
		complete = len(first.rows) < pageSize
		for p := 2; !complete && p <= maxPages; p++ {
			pagesRead++
			next, err := c.readSettlementPage(ctx, notification, p, perPage)
			if err != nil {
				return nil, err
			}
			if !accept(p, next) {
				break
			}
			complete = len(next.rows) < pageSize
		}
	}

	if len(listing.Rows) > maxRows {
		listing.Rows = listing.Rows[:maxRows]
		complete = false
	}
	if listing.Total > 0 && len(listing.Rows) != listing.Total {
		complete = false
	}
	if listing.RepeatedRows > 0 {
		complete = false
		c.logger.WarnContext(ctx, "Singapay listed a transaction id more than once in one settlement",
			"settlement_notification_uuid", notification.UUID,
			"settlement_id", notification.SettlementID,
			"repeated_rows", listing.RepeatedRows,
		)
	}
	listing.Complete = complete

	c.logger.InfoContext(ctx, "Read a settlement's listing",
		"settlement_notification_uuid", notification.UUID,
		"settlement_id", notification.SettlementID,
		"settlement_reference", notification.SettlementReference,
		"pages", pagesRead,
		"rows", len(listing.Rows),
		"total", listing.Total,
		"complete", listing.Complete,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return listing, nil
}

// MatchSettlementTransactions puts each row of a settlement's listing beside the ledger's
// record of the same payment, as ListSettlementTransactions does a page: by invoice number,
// then by a stored gateway identifier, else unmatched and kept — each with the account it
// belongs to. The rows keep their order. Read only.
//
// Errors: CodeDatabaseError.
func (c *LedgerClient) MatchSettlementTransactions(ctx context.Context, rows []SettlementGatewayTransaction) ([]SettlementTransaction, error) {
	matched := make([]SettlementTransaction, len(rows))
	for i, row := range rows {
		matched[i] = SettlementTransaction{Gateway: row, Match: SettlementMatchNone}
	}
	if err := c.matchSettlementTransactions(ctx, matched); err != nil {
		return nil, err
	}
	if err := c.attachSettlementAccounts(ctx, matched); err != nil {
		return nil, err
	}
	return matched, nil
}

// settlementNotification reads the stored notification a listing is read through.
func (c *LedgerClient) settlementNotification(ctx context.Context, notificationID string) (*domain.SettlementNotification, error) {
	if notificationID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest, "settlement notification id is required", nil)
	}
	notification, err := c.repoProvider.SettlementNotification().GetByID(ctx, notificationID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrSettlementNotificationNotFound.WithError(err)
		}
		return nil, ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the settlement notification", err)
	}
	return notification, nil
}

// settlementPage is one page of a settlement's listing as Singapay answered it.
type settlementPage struct {
	rows       []singapay.SettlementTransaction
	pagination singapay.Pagination
}

// readSettlementPage asks Singapay for one page of the settlement's listing. Its 404 is
// ErrSettlementNotListed — the batch is not completed, or not this merchant's — and
// anything else it fails with is CodeGatewayAPIError.
func (c *LedgerClient) readSettlementPage(ctx context.Context, notification *domain.SettlementNotification, page, perPage int) (settlementPage, error) {
	rows, pagination, err := c.gateway.ListSettlementTransactions(ctx, notification.SettlementID,
		singapay.ListSettlementTransactionsOptions{Page: page, PerPage: perPage})
	if err != nil {
		if gatewayErr, ok := singapay.AsError(err); ok && gatewayErr.StatusCode == http.StatusNotFound {
			return settlementPage{}, ledgererr.ErrSettlementNotListed.WithError(err)
		}
		return settlementPage{}, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "could not read the settlement's transactions from Singapay", err)
	}
	return settlementPage{rows: rows, pagination: pagination}, nil
}

// readSettlementPages reads pages from … to, settlementListingConcurrency at a time, and
// returns them in order. The first page to fail cancels the pages still in flight; its error
// — not the cancellation it caused — is the one returned. A read that panics is an error
// like any other: it runs off the caller's goroutine, where nothing would recover it.
func (c *LedgerClient) readSettlementPages(ctx context.Context, notification *domain.SettlementNotification, from, to, perPage int) ([]settlementPage, error) {
	if to < from {
		return nil, nil
	}
	pages := make([]settlementPage, to-from+1)
	errs := make([]error, len(pages))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	slots := make(chan struct{}, settlementListingConcurrency)
	var wg sync.WaitGroup
	for p := from; p <= to && ctx.Err() == nil; p++ {
		slots <- struct{}{}
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if r := recover(); r != nil {
					errs[p-from] = ledgererr.NewError(ledgererr.CodeInternal, "reading a page of the settlement's transactions failed",
						fmt.Errorf("page %d: panic: %v", p, r))
					cancel()
				}
			}()
			page, err := c.readSettlementPage(ctx, notification, p, perPage)
			if err != nil {
				errs[p-from] = err
				cancel()
				return
			}
			pages[p-from] = page
		}(p)
	}
	wg.Wait()

	var cancelled error
	for _, err := range errs {
		switch {
		case err == nil:
		case !errors.Is(err, context.Canceled):
			return nil, err
		case cancelled == nil:
			cancelled = err
		}
	}
	if cancelled != nil {
		return nil, cancelled
	}
	if err := ctx.Err(); err != nil {
		// The caller's context ended between launching the reads: some pages were never read.
		return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "could not read the settlement's transactions from Singapay", err)
	}
	return pages, nil
}

// settlementGatewayTransaction converts a listed row to the ledger's units.
func settlementGatewayTransaction(row singapay.SettlementTransaction) SettlementGatewayTransaction {
	t := SettlementGatewayTransaction{
		TransactionID:     row.TransactionID,
		MerchantReference: row.MerchantReffNo,
		PaymentMethod:     row.Fees.Name,
		AmountMinor:       row.Amount.Minor(),
		Currency:          cmp.Or(row.Amount.Currency, row.Fees.Currency),
		PostedAt:          millisTimePtr(row.PostedAt),
		PaidAt:            millisTimePtr(row.ProcessedAt),
		Settled:           row.HasSettle,
		SettledAt:         millisTimePtr(row.SettleAt),
	}
	if row.Account != nil {
		t.AccountID = row.Account.ID
		t.AccountName = row.Account.Name
	}
	if row.Fees.Amount.Set {
		fee := row.Fees.Amount.Minor()
		t.FeeMinor = &fee
	}
	return t
}

// matchSettlementTransactions ties each listed row to a ledger transaction: by invoice
// number in one query, then — for the rows that found nothing — by stored gateway
// identifier in two more. Rows that match neither keep SettlementMatchNone.
//
// The invoice lookup takes each row's transaction_id as well as its merchant_reff_no. For a
// payment link Singapay's documentation does not say which reference lands where — the link
// is created with the invoice as reff_no, and "other types use reff_no" for transaction_id —
// and an attempt the money-in webhook left without its own reference has no gateway
// identifier stored to be found by.
func (c *LedgerClient) matchSettlementTransactions(ctx context.Context, rows []SettlementTransaction) error {
	invoices := distinctNonEmpty(2*len(rows), func(i int) string {
		if i%2 == 0 {
			return rows[i/2].Gateway.MerchantReference
		}
		return rows[i/2].Gateway.TransactionID
	})
	byInvoice := map[string]*domain.ProductTransaction{}
	if len(invoices) > 0 {
		txs, err := c.repoProvider.ProductTransaction().GetByInvoiceNumbers(ctx, invoices)
		if err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the settlement's transactions by invoice number", err)
		}
		for _, tx := range txs {
			byInvoice[tx.InvoiceNumber] = preferredTransaction(byInvoice[tx.InvoiceNumber], tx)
		}
	}
	for i := range rows {
		tx := byInvoice[rows[i].Gateway.MerchantReference]
		if tx == nil {
			tx = byInvoice[rows[i].Gateway.TransactionID]
		}
		if tx != nil {
			rows[i].Transaction = tx
			rows[i].Match = SettlementMatchInvoice
		}
	}

	gatewayIDs := distinctNonEmpty(len(rows), func(i int) string {
		if rows[i].Match != SettlementMatchNone {
			return ""
		}
		return rows[i].Gateway.TransactionID
	})
	if len(gatewayIDs) == 0 {
		return nil
	}

	requests, err := c.repoProvider.PaymentRequest().GetByGatewayIdentifiers(ctx, gatewayIDs)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read payment requests by gateway identifier", err)
	}
	txIDs := distinctNonEmpty(len(requests), func(i int) string { return requests[i].ProductTransactionUUID })
	if len(txIDs) == 0 {
		return nil
	}
	txs, err := c.repoProvider.ProductTransaction().GetByIDs(ctx, txIDs)
	if err != nil {
		return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read the settlement's transactions by id", err)
	}
	byID := make(map[string]*domain.ProductTransaction, len(txs))
	for _, tx := range txs {
		byID[tx.UUID] = tx
	}

	// One identifier can only be one payment at Singapay, but the ledger keeps two columns
	// per request and makes no uniqueness claim about either; if two requests answer, the
	// paid one wins, exactly as for a reused invoice number.
	byGatewayID := map[string]*domain.ProductTransaction{}
	for _, pr := range requests {
		tx := byID[pr.ProductTransactionUUID]
		if tx == nil {
			continue
		}
		for _, id := range []string{pr.GatewayTransactionRef, pr.GatewayTransactionID} {
			if id != "" {
				byGatewayID[id] = preferredTransaction(byGatewayID[id], tx)
			}
		}
	}
	for i := range rows {
		if rows[i].Match != SettlementMatchNone {
			continue
		}
		if tx := byGatewayID[rows[i].Gateway.TransactionID]; tx != nil {
			rows[i].Transaction = tx
			rows[i].Match = SettlementMatchGatewayID
		}
	}
	return nil
}

// attachSettlementAccounts names each row's ledger accounts: the sub-account's holder, and
// the account the payment belongs to — the matched transaction's seller, else the
// sub-account's holder. A settlement involves far fewer accounts than rows, so each distinct
// account is read once.
func (c *LedgerClient) attachSettlementAccounts(ctx context.Context, rows []SettlementTransaction) error {
	byID := map[string]*domain.Account{}
	bySingapayID := map[string]*domain.Account{}

	for i := range rows {
		var seller *domain.Account
		if tx := rows[i].Transaction; tx != nil && tx.SellerAccountID != "" {
			found, ok := byID[tx.SellerAccountID]
			if !ok {
				var err error
				found, err = c.repoProvider.Account().GetByID(ctx, tx.SellerAccountID)
				if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
					return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read a seller account", err)
				}
				byID[tx.SellerAccountID] = found
			}
			seller = found
		}

		var sub *domain.Account
		if singapayID := rows[i].Gateway.AccountID; singapayID != "" {
			found, ok := bySingapayID[singapayID]
			switch {
			case ok:
			case seller != nil && seller.SingapayAccountID == singapayID:
				found = seller
			default:
				var err error
				found, err = c.repoProvider.Account().GetBySingapayAccountID(ctx, singapayID)
				if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
					return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to read an account by its Singapay sub-account", err)
				}
			}
			bySingapayID[singapayID] = found
			sub = found
		}

		rows[i].SubAccount = sub
		rows[i].Account = cmp.Or(seller, sub)
	}
	return nil
}

// preferredTransaction picks between two transactions answering to one reference: the one
// that was paid — SETTLED, then COMPLETED — and between equals the newer. A nil current
// loses to anything.
func preferredTransaction(current, candidate *domain.ProductTransaction) *domain.ProductTransaction {
	if current == nil {
		return candidate
	}
	rank := func(tx *domain.ProductTransaction) int {
		switch tx.Status {
		case domain.TransactionStatusSettled:
			return 2
		case domain.TransactionStatusCompleted:
			return 1
		default:
			return 0
		}
	}
	if rc, rn := rank(current), rank(candidate); rc != rn {
		if rn > rc {
			return candidate
		}
		return current
	}
	if candidate.CreatedAt.After(current.CreatedAt) {
		return candidate
	}
	return current
}

// distinctNonEmpty collects key(0) … key(n-1), skipping empty strings and repeats, in order.
func distinctNonEmpty(n int, key func(i int) string) []string {
	seen := make(map[string]bool, n)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k := key(i)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}
