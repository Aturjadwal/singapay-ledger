// Package singapay is a client for the Singapay Payment Gateway merchant API.
//
// It covers the surface this ledger needs to replace DOKU: sub-accounts, balances,
// transfers between accounts, and bank disbursement — plus the three signature schemes
// Singapay uses. Money-in (payment link, virtual account, QRIS) and reconciliation are
// deliberately absent for now; see docs/105-singapay-migration.md for why those are still
// open questions.
//
// Nothing here touches the ledger domain. This package speaks HTTP and returns Singapay's
// own shapes, so the decisions the migration has not settled — how a payout fee is
// booked, what a settlement batch means for pending balances — stay where they belong.
//
// # Versions are per module, not per API
//
// Singapay versions each module separately: accounts, balances, statements and account
// transfer exist only at v1.0, while disbursement and card exist at v2.0. There is no
// "API v2" that supersedes v1. Mixing is unavoidable and expected — this package writes
// through the highest version available and reads through whatever exists.
//
// # Two envelopes
//
// v1.0 endpoints answer with {status, success, data}; v2.0 endpoints answer with
// {response_code, response_message, data} carrying an SP000–SP020 code. Both are handled,
// and both collapse into the same [Error] on failure.
//
// # Amounts
//
// Singapay is inconsistent about how it writes money: "1234.56" on balances, 100000 on a
// VA webhook, "500000" on an account transfer. Every monetary field here is an [Amount],
// which accepts all three and refuses to truncate silently. Reading a balance with
// fmt.Sscanf("%d") — which is what the DOKU path does today — turns "1234.56" into 1234
// and reports no error at all.
//
// # A response code is not a transaction status
//
// SP000 means the request was accepted, not that money moved. The payment outcome lives
// in [TransactionStatus] (00–07). Conflating the two is the documented mistake, and for a
// payout it is the expensive one: see [Outcome] for why "the request failed" and "the
// money definitely did not move" are different claims.
package singapay
