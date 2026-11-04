package singapay

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ewalletOrderBody is a create-order data object as Singapay answered it on 2026-11-04, as
// far as the failure showed: account_id is a number. {{account_id}} and {{has_settle}} are
// replaced per case.
const ewalletOrderBody = `{
	"id":42,"account_id":{{account_id}},"reff_no":"EW-1","merchant_reff_no":"INV-2026-001",
	"status":"pending","status_computed":"pending","is_expired":false,
	"ewallet_vendor":"EWALLET_SHOPEEPAY",
	"amount":95000,"total_amount":100000,"merchant_fee":5000,"net_amount":95000,
	"checkout_url":"https://pay.example.com/x","checkout_url_app":null,
	"has_settle":{{has_settle}},"settle_at":null,
	"expired_at":"2026-11-04T21:58:20+07:00","created_at":"2026-11-04T20:58:20+07:00"}`

func ewalletOrder(accountID, hasSettle string) string {
	return strings.NewReplacer("{{account_id}}", accountID, "{{has_settle}}", hasSettle).Replace(ewalletOrderBody)
}

func TestEwalletTransactionReadsAccountIDAsNumberOrString(t *testing.T) {
	tests := []struct {
		name      string
		accountID string
		want      string
	}{
		// What Singapay sends: its own id for the sub-account.
		{"a number", `35`, "35"},
		{"a ULID string", `"01K9ABCDEF"`, "01K9ABCDEF"},
		{"null", `null`, ""},
		// Nothing acts on account_id, so even a shape nobody expects must not cost the
		// order that Singapay has already opened.
		{"an object", `{"id":35}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tx EwalletTransaction
			if err := json.Unmarshal([]byte(ewalletOrder(tt.accountID, `false`)), &tx); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if tx.AccountID != tt.want {
				t.Errorf("AccountID = %q, want %q", tx.AccountID, tt.want)
			}
			// What the ledger needs from a new order is all there.
			if tx.ID != 42 || tx.CheckoutURL != "https://pay.example.com/x" {
				t.Errorf("ID = %d, CheckoutURL = %q", tx.ID, tx.CheckoutURL)
			}
		})
	}
}

func TestEwalletTransactionReadsFlagsWrittenAsNumbers(t *testing.T) {
	tests := []struct {
		hasSettle string
		want      bool
	}{
		{`true`, true},
		{`false`, false},
		{`1`, true},
		{`0`, false},
		{`"1"`, true},
		{`"0"`, false},
		{`"true"`, true},
		{`null`, false},
		{`""`, false},
	}
	for _, tt := range tests {
		t.Run(tt.hasSettle, func(t *testing.T) {
			var tx EwalletTransaction
			if err := json.Unmarshal([]byte(ewalletOrder(`35`, tt.hasSettle)), &tx); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if tx.HasSettle != tt.want {
				t.Errorf("HasSettle = %v, want %v", tx.HasSettle, tt.want)
			}
		})
	}

	t.Run("is_expired too", func(t *testing.T) {
		body := strings.Replace(ewalletOrder(`35`, `0`), `"is_expired":false`, `"is_expired":1`, 1)
		var tx EwalletTransaction
		if err := json.Unmarshal([]byte(body), &tx); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !tx.IsExpired {
			t.Error("IsExpired = false, want true for 1")
		}
	})

	t.Run("absent is false", func(t *testing.T) {
		var tx EwalletTransaction
		if err := json.Unmarshal([]byte(`{"id":42,"checkout_url":"https://pay.example.com/x"}`), &tx); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if tx.HasSettle || tx.IsExpired {
			t.Errorf("HasSettle = %v, IsExpired = %v, want both false", tx.HasSettle, tx.IsExpired)
		}
	})
}

// has_settle decides whether a payment has settled, so a value that is no boolean at all is
// refused rather than read as false.
func TestEwalletTransactionRefusesAFlagThatIsNoBoolean(t *testing.T) {
	for _, hasSettle := range []string{`"yes"`, `2`, `{}`} {
		var tx EwalletTransaction
		err := json.Unmarshal([]byte(ewalletOrder(`35`, hasSettle)), &tx)
		if err == nil || !strings.Contains(err.Error(), "has_settle") {
			t.Errorf("has_settle %s: err = %v, want an error naming has_settle", hasSettle, err)
		}
	}
}

// Only account_id and the two flags are read more widely. Every other field keeps its type's
// own rules: an amount with a real third decimal and a timestamp that is not ISO 8601 are
// still refused.
func TestEwalletTransactionKeepsTheOtherFieldsStrict(t *testing.T) {
	tests := []struct {
		name, from, to string
		// mention is what the error must say, so a log names what Singapay sent.
		mention string
	}{
		{"an amount past the sen", `"merchant_fee":5000`, `"merchant_fee":5000.125`, "5000.125"},
		{"a timestamp that is not ISO 8601", `"expired_at":"2026-11-04T21:58:20+07:00"`, `"expired_at":"besok"`, "besok"},
		{"an id that is not a number", `"id":42`, `"id":"EW-42"`, "id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := ewalletOrder(`35`, `false`)
			if !strings.Contains(body, tt.from) {
				t.Fatalf("fixture does not contain %s", tt.from)
			}
			var tx EwalletTransaction
			err := json.Unmarshal([]byte(strings.Replace(body, tt.from, tt.to, 1)), &tx)
			if err == nil {
				t.Fatalf("want %s refused, got %+v", tt.to, tx)
			}
			if !strings.Contains(err.Error(), "e-wallet transaction") || !strings.Contains(err.Error(), tt.mention) {
				t.Errorf("err = %q, want it to name the e-wallet transaction and %q", err, tt.mention)
			}
		})
	}
}

// The list and the inquiry decode the same type, so the reading above holds for them too.
func TestListEwalletTransactionsReadsNumericAccountIDs(t *testing.T) {
	body := `{"status":200,"success":true,"data":[` + ewalletOrder(`35`, `1`) + `],
		"pagination":{"count":1,"total":1,"per_page":25,"current_page":1,"total_pages":1}}`
	srv := newServer(t, nil, func(w http.ResponseWriter, r *http.Request, c *capture) {
		writeJSON(w, 200, body)
	})
	c := testClient(t, srv.URL)

	txs, _, err := c.ListEwalletTransactions(context.Background(), "01K9", SettlementWindow{})
	if err != nil {
		t.Fatalf("ListEwalletTransactions: %v", err)
	}
	if len(txs) != 1 {
		t.Fatalf("got %d transactions, want 1", len(txs))
	}
	if txs[0].AccountID != "35" || !txs[0].HasSettle {
		t.Errorf("AccountID = %q, HasSettle = %v, want \"35\" and true", txs[0].AccountID, txs[0].HasSettle)
	}
}
