package singapay

import (
	"testing"
	"time"
)

// The four money-in payloads below are Singapay's documented samples, verbatim. They are
// the reason this package does not assume the four channels agree about anything.

const vaWebhook = `{
  "status": 200, "success": true, "event": "va-transaction", "timestamp": "26 Dec 2025 13:35:45",
  "data": {
    "transaction": {
      "reff_no": "INV-2026-001",
      "transaction_id": "3211120250926133543246",
      "type": "va", "status": "paid",
      "amount": { "value": 100000, "currency": "IDR" },
      "amount_type": "closed", "tip": null,
      "post_timestamp": "26 Dec 2025 13:35:43",
      "processed_timestamp": "26 Dec 2025 13:35:45"
    },
    "payment": { "method": "va", "additional_info": {
      "va_number": "7872955146576837", "va_name": "Toko Budi",
      "bank": { "short_name": "Maybank", "number": "016", "swift_code": "IBBKIDJA", "bank_code": "MAYBANK" },
      "fees": { "name": "VA Maybank", "amount": 1500, "currency": "IDR" } } }
  }
}`

// Note: no "event" field. This is exactly as documented, and routing on event alone
// would drop it.
const paymentLinkWebhook = `{
    "status": 200, "success": true,
    "data": {
        "transaction": {
            "reff_no": "18917720251110094037705",
            "type": "pl", "status": "paid",
            "amount": { "value": "10000.00", "currency": "IDR" },
            "tip": null,
            "post_timestamp": "10 Nov 2025 09:46:38",
            "processed_timestamp": "10 Nov 2025 09:46:38"
        },
        "customer": { "id": null, "name": "Mohammad Zulkifli Katili", "email": "moh@example.com", "phone": "082291501085" },
        "payment": { "method": "payment_link", "additional_info": { "payment_link": {
            "id": 189, "reff_no": "PL20251105160923690b1443a67e9", "title": "tes",
            "payment_date": "2025-11-05T09:09:49.000000Z",
            "payment_url": "https://sandbox-paymentlink.singapay.id/b2b/PL20251105160923690b1443a67e9",
            "status": "open", "max_usage": 1000, "current_usage": 4,
            "expired_at": null, "total_amount": "10000.00", "account_id": 35 } } }
    }
}`

const qrisWebhook = `{
  "status": 200, "success": true, "event": "qris-acquirer-transaction", "timestamp": "26 Dec 2025 13:31:59",
  "data": {
    "transaction": {
      "id": 42, "reff_no": "6601K62BH34X445J046C4W5249E6", "merchant_reff_no": "INV-2026-001",
      "type": "qris", "status": "paid",
      "amount": { "value": 1000123, "currency": "IDR" },
      "tip": { "value": 0, "currency": "IDR" },
      "total_amount": { "value": 1000123, "currency": "IDR" },
      "post_timestamp": "26 Dec 2025 13:31:59", "processed_timestamp": "26 Dec 2025 13:31:59"
    },
    "customer": { "id": "01K2KVRQQP45234X9T3YWG1FKT", "name": "Moh. Zulkifli Katili" },
    "payment": { "method": "qris", "additional_info": { "qr_string": "00020101021226570015ID.SINGAPAY.WWW", "payment_event_id": 12345 } }
  }
}`

const ewalletWebhook = `{
  "status": 200, "success": true, "event": "ewallet-native-transaction", "timestamp": "26 Dec 2025 13:35:45",
  "data": {
    "transaction": {
      "id": 42, "reff_no": "INV-2026-001", "merchant_reff_no": "INV-2026-001",
      "type": "ewallet", "ewallet_vendor": "GOPAY", "status": "paid",
      "amount": { "value": 95000, "currency": "IDR" },
      "total_amount": { "value": 100000, "currency": "IDR" },
      "post_timestamp": "26 Dec 2025 13:35:43", "processed_timestamp": "26 Dec 2025 13:35:45"
    },
    "customer": { "name": "John Doe", "email": "john@example.com", "phone": "081234567890" },
    "payment": { "method": "ewallet", "vendor": "GOPAY",
      "additional_info": { "payment_event_id": 1042, "vendor_reference_no": "PAY-XYZ-12345" } }
  }
}`

func TestParseMoneyInKind(t *testing.T) {
	tests := []struct {
		name string
		body string
		want MoneyInEvent
	}{
		{"virtual account", vaWebhook, EventVATransaction},
		{"qris", qrisWebhook, EventQRISAcquirer},
		{"e-wallet", ewalletWebhook, EventEwalletNative},
		// The documented payment-link sample carries no event field at all.
		{"payment link, no event field", paymentLinkWebhook, EventPaymentLink},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseMoneyInNotification([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := n.Kind(); got != tc.want {
				t.Errorf("Kind() = %q, want %q", got, tc.want)
			}
			if !n.IsPaid() {
				t.Error("IsPaid() = false, want true")
			}
		})
	}
}

// TestMerchantReference pins the rule that differs per channel, and the payment-link case
// is the one that matters: its transaction.reff_no is an attempt id, so matching an
// invoice on it would never hit.
func TestMerchantReference(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"VA carries it in reff_no", vaWebhook, "INV-2026-001"},
		{"QRIS carries it in merchant_reff_no", qrisWebhook, "INV-2026-001"},
		{"e-wallet carries it in merchant_reff_no", ewalletWebhook, "INV-2026-001"},
		{"payment link carries it on the link, not the transaction", paymentLinkWebhook, "PL20251105160923690b1443a67e9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseMoneyInNotification([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if got := n.MerchantReference(); got != tc.want {
				t.Errorf("MerchantReference() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPaymentLinkReferenceIsNotTheAttemptID(t *testing.T) {
	n, err := ParseMoneyInNotification([]byte(paymentLinkWebhook))
	if err != nil {
		t.Fatal(err)
	}
	if n.MerchantReference() == n.Data.Transaction.ReffNo {
		t.Error("returned the attempt id; an invoice lookup on that never matches")
	}
}

func TestMoneyInAmounts(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantCharged int64 // rupiah
	}{
		// A bare JSON number.
		{"VA", vaWebhook, 100000},
		// A quoted decimal for the same kind of field.
		{"payment link", paymentLinkWebhook, 10000},
		// total_amount is amount plus tip here.
		{"QRIS", qrisWebhook, 1000123},
		// total_amount is the gross against a net amount here — a different meaning
		// for the same two field names.
		{"e-wallet", ewalletWebhook, 100000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseMoneyInNotification([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			got, err := n.Charged().Rupiah()
			if err != nil {
				t.Fatalf("Charged(): %v", err)
			}
			if got != tc.wantCharged {
				t.Errorf("Charged() = %d, want %d", got, tc.wantCharged)
			}
		})
	}
}

func TestChannelFee(t *testing.T) {
	t.Run("VA reports its fee", func(t *testing.T) {
		n, err := ParseMoneyInNotification([]byte(vaWebhook))
		if err != nil {
			t.Fatal(err)
		}
		fee, ok := n.ChannelFee()
		if !ok {
			t.Fatal("want a fee")
		}
		got, err := fee.Rupiah()
		if err != nil || got != 1500 {
			t.Errorf("fee = %d, %v; want 1500", got, err)
		}
	})

	// The gap that makes a fee-mismatch reconciliation impossible on payment links.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"payment link reports none", paymentLinkWebhook},
		{"QRIS reports none on the webhook", qrisWebhook},
		{"e-wallet reports none on the webhook", ewalletWebhook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := ParseMoneyInNotification([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := n.ChannelFee(); ok {
				t.Error("want no fee reported")
			}
		})
	}
}

func TestChannelInfoAccessors(t *testing.T) {
	t.Run("VA", func(t *testing.T) {
		n, _ := ParseMoneyInNotification([]byte(vaWebhook))
		info, err := n.VAInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.VANumber != "7872955146576837" || info.Bank.SwiftCode != "IBBKIDJA" {
			t.Errorf("got %+v", info)
		}
	})

	t.Run("QRIS", func(t *testing.T) {
		n, _ := ParseMoneyInNotification([]byte(qrisWebhook))
		info, err := n.QRISInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.PaymentEventID != 12345 || info.QRString == "" {
			t.Errorf("got %+v", info)
		}
	})

	t.Run("e-wallet", func(t *testing.T) {
		n, _ := ParseMoneyInNotification([]byte(ewalletWebhook))
		info, err := n.EwalletInfo()
		if err != nil {
			t.Fatal(err)
		}
		if info.VendorReferenceNo != "PAY-XYZ-12345" {
			t.Errorf("got %+v", info)
		}
	})

	t.Run("wrong accessor refuses", func(t *testing.T) {
		n, _ := ParseMoneyInNotification([]byte(qrisWebhook))
		if _, err := n.VAInfo(); err == nil {
			t.Error("want an error reading VA detail off a QRIS webhook")
		}
	})
}

func TestMoneyInTimestamps(t *testing.T) {
	n, err := ParseMoneyInNotification([]byte(vaWebhook))
	if err != nil {
		t.Fatal(err)
	}
	ts := n.Data.Transaction.ProcessedAt
	if !ts.Set {
		t.Fatal("processed_timestamp not parsed")
	}
	// "26 Dec 2025 13:35:45" read in Asia/Jakarta.
	want := time.Date(2025, 12, 26, 13, 35, 45, 0, jakarta)
	if !ts.Equal(want) {
		t.Errorf("got %s, want %s", ts.Time, want)
	}
}

func TestParseMoneyInRejectsUnknownShape(t *testing.T) {
	if _, err := ParseMoneyInNotification([]byte(`{"status":200,"data":{}}`)); err == nil {
		t.Error("want an error when neither event nor payment method identifies the channel")
	}
}

func TestParseMoneyOutNotification(t *testing.T) {
	body := `{"response_code":"SP000","response_message":"Successfully","event":"disbursement",
	  "data":{"transaction_id":"1012","reference_number":"11111111118",
	  "transaction_status":{"code":"00","desc":"Success"},
	  "post_timestamp":"1766978961000","processed_timestamp":"1766978962000",
	  "gross_amount":{"currency":"IDR","value":"12504.00"},
	  "fee":{"currency":"IDR","value":"2500"},
	  "net_amount":{"currency":"IDR","value":"10004.00"}}}`

	n, err := ParseMoneyOutNotification([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if n.Event != EventDisbursement {
		t.Errorf("Event = %q", n.Event)
	}
	if !n.Succeeded() {
		t.Error("want success")
	}
	// Money-out webhooks use epoch milliseconds where money-in uses text.
	if !n.Data.PostedAt.Set || n.Data.PostedAt.Unix() != 1766978961 {
		t.Errorf("post_timestamp = %+v", n.Data.PostedAt)
	}
}

func TestParseMoneyOutFailureCarriesReason(t *testing.T) {
	n, err := ParseMoneyOutNotification([]byte(disburseFailedBody))
	if err != nil {
		t.Fatal(err)
	}
	if n.Succeeded() {
		t.Error("want failure")
	}
	if !n.Data.TransactionStatus().Failed() {
		t.Error("06 must report as failed")
	}
	if n.Data.FailedReason == "" {
		t.Error("want a failure reason")
	}
}

const settlementCompletedWebhook = `{
  "status": 200, "success": true, "event": "settlement.completed", "timestamp": "18 Jun 2026 10:00:00",
  "data": {
    "settlement": {
      "id": 1234, "reference_no": "SETTLEMENT-1-ABC123",
      "title": "Settlement Acme (01 Jun 2026 - 17 Jun 2026)",
      "status": "completed", "settlement_type": "ALL", "settlement_method": "balance",
      "is_auto_created": false,
      "start_date": "01 Jun 2026 00:00:00", "end_date": "17 Jun 2026 23:59:59",
      "amount": 1000000, "total_admin_fee": 5000, "total_vendor_fee": 3000,
      "total_our_margin": 2000, "settlement_fee": 0, "total_to_transfer": 1000000,
      "total_refunded": 0, "currency": "IDR", "transfer_status": null,
      "approved_by": "Jane Finance", "approved_at": "18 Jun 2026 10:00:00",
      "recipient": { "bank_code": null, "account_number": null, "account_name": null }
    },
    "total_transactions": 5
  }
}`

func TestParseSettlementNotification(t *testing.T) {
	n, err := ParseSettlementNotification([]byte(settlementCompletedWebhook))
	if err != nil {
		t.Fatal(err)
	}
	if n.Event != EventSettlementCompleted {
		t.Errorf("Event = %q", n.Event)
	}
	s := n.Data.Settlement
	if s.ReferenceNo != "SETTLEMENT-1-ABC123" {
		t.Errorf("ReferenceNo = %q", s.ReferenceNo)
	}
	if n.Data.TotalTransactions != 5 {
		t.Errorf("TotalTransactions = %d", n.Data.TotalTransactions)
	}

	// The method decides whether this moves pending into available or pays out to a
	// bank — a different ledger consequence entirely.
	if !s.MovesPendingToAvailable() {
		t.Error("settlement_method balance must move pending to available")
	}

	// The batch reports a date range but no transaction list. Reconstructing the rows
	// means querying each product's transactions across this window.
	if !s.StartDate.Set || !s.EndDate.Set {
		t.Error("the batch window must parse — it is the only handle on which rows it covered")
	}
	if amount, err := s.Amount.Rupiah(); err != nil || amount != 1000000 {
		t.Errorf("Amount = %d, %v", amount, err)
	}
}

func TestSettlementMethodDecidesBalanceMovement(t *testing.T) {
	tests := []struct {
		method SettlementMethod
		want   bool
	}{
		{SettlementToBalance, true},
		{SettlementAutoBalance, true},
		{SettlementToBank, false},
		{SettlementToEwallet, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.method), func(t *testing.T) {
			s := Settlement{Method: tc.method}
			if got := s.MovesPendingToAvailable(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
