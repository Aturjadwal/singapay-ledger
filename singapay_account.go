package ledger

import "strings"

const (
	// maxSubAccountNameLength bounds the name sent to POST /api/v1.0/accounts.
	//
	// Singapay documents no limit — unlike the previous gateway, it does not identify a
	// sub-account by email and imposes no length rule on the name. A bound is kept
	// anyway because this call sits inside a seller's first paid booking, and an
	// unbounded user-supplied string reaching a payment API is the kind of thing that
	// fails once, in production, on somebody's display name.
	maxSubAccountNameLength = 100

	// defaultSubAccountName is used when a user's name is blank or whitespace. Singapay
	// needs a name; failing a paid booking over a display label would be the wrong
	// trade.
	defaultSubAccountName = "Seller"

	// platformFeeTransferPrefix namespaces the merchant reference of a platform-fee
	// transfer so it cannot collide with the payment it is derived from.
	//
	// Singapay's merchant_ref_no is unique per merchant across *every* account
	// movement, and re-sending a used value returns the original transfer and moves
	// nothing further. That makes the reference the entire idempotency mechanism, so it
	// must be deterministic: derived from the invoice, never random per attempt. A
	// random value would forfeit replay protection without surfacing any error at all.
	platformFeeTransferPrefix = "PF-"
)

// platformFeeTransferReference derives the transfer's merchant reference from the
// payment's invoice number. INV-20060102150405-XXXXXX (25 chars) + "PF-" = 28.
func platformFeeTransferReference(paymentInvoiceNumber string) string {
	return platformFeeTransferPrefix + paymentInvoiceNumber
}

// sanitizeSubAccountName trims a user-supplied name to something safe to send.
//
// Singapay accepts far more than the previous gateway did — digits and punctuation are
// fine, so "PT. Maju 123" survives intact — so this only collapses whitespace, bounds the
// length, and substitutes a default for a name that is empty once trimmed.
func sanitizeSubAccountName(name string) string {
	sanitized := strings.Join(strings.Fields(name), " ")
	if sanitized == "" {
		return defaultSubAccountName
	}
	if len(sanitized) > maxSubAccountNameLength {
		sanitized = strings.TrimSpace(truncateRunes(sanitized, maxSubAccountNameLength))
	}
	if sanitized == "" {
		return defaultSubAccountName
	}
	return sanitized
}

// truncateRunes cuts to at most max bytes without splitting a multi-byte rune — the
// length a gateway counts is the one it receives, so the budget is in bytes.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Sub-account creation takes no email. Singapay identifies an account by its ULID, and
// the one field that accepts an address — invite_members — only accepts addresses that
// already belong to a member of our merchant; anything else is a 422. A seller's own
// address never qualifies, so there is nothing to validate here any more.
