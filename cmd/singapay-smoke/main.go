// Command singapay-smoke exercises the Singapay client against a live environment.
//
// It exists to answer the questions no unit test can: do our credentials work, is our IP
// allowed, and — the one that blocks everything else — which of the two documented
// X-Timestamp formats does the signature middleware actually accept.
//
// Credentials come from the environment:
//
//	SINGAPAY_CLIENT_ID, SINGAPAY_CLIENT_SECRET, SINGAPAY_PARTNER_ID
//
// Usage:
//
//	go run ./cmd/singapay-smoke                        # read-only battery
//	go run ./cmd/singapay-smoke -step signature        # resolve the X-Timestamp question
//	go run ./cmd/singapay-smoke -step beneficiary -bank 002 -account 1234567890
//	go run ./cmd/singapay-smoke -step fee -account-id 01K9... -swift BRINIDJA -amount 50000
//	go run ./cmd/singapay-smoke -step create-account -name "Budi Santoso"
//
// Defaults to sandbox. -production must be passed explicitly, and the only step that
// writes anything is create-account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Aturjadwal/singapay-ledger/singapay"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "\n✘ "+err.Error())
		os.Exit(1)
	}
}

type options struct {
	step        string
	production  bool
	timestamp   string
	name        string
	bank        string
	account     string
	accountID   string
	swift       string
	amount      int64
	timeoutSecs int
	baseURL     string
	reff        string
	bankVA      string

	// verify-webhook inputs: a delivery captured off the wire, replayed against each
	// candidate key.
	webhookFile      string
	webhookEndpoint  string
	webhookSignature string
	webhookTimestamp string
	webhookAuth      string
}

func run(args []string) error {
	fs := flag.NewFlagSet("singapay-smoke", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.step, "step", "readonly", "readonly | token | balance | accounts | methods | signature | verify-webhook | beneficiary | fee | create-account | payment-link | va | qris")
	fs.StringVar(&o.webhookFile, "webhook-body", "", "verify-webhook: file holding the raw webhook body, byte for byte as received")
	fs.StringVar(&o.webhookEndpoint, "webhook-endpoint", "/singapay/notification", "verify-webhook: the path Singapay signed")
	fs.StringVar(&o.webhookSignature, "webhook-signature", "", "verify-webhook: the X-Signature header from the delivery")
	fs.StringVar(&o.webhookTimestamp, "webhook-timestamp", "", "verify-webhook: the X-Timestamp header from the delivery")
	fs.StringVar(&o.webhookAuth, "webhook-authorization", "", "verify-webhook: the Authorization header from the delivery")
	fs.BoolVar(&o.production, "production", false, "target production instead of sandbox")
	fs.StringVar(&o.timestamp, "timestamp", "unix", "X-Timestamp format: unix | iso")
	fs.StringVar(&o.name, "name", "", "create-account: the sub-account name")
	fs.StringVar(&o.bank, "bank", "", "beneficiary: bank code (3-digit or SWIFT)")
	fs.StringVar(&o.account, "account", "", "beneficiary: bank account number")
	fs.StringVar(&o.accountID, "account-id", "", "fee: sub-account ULID to quote against")
	fs.StringVar(&o.swift, "swift", "", "fee: destination bank SWIFT code")
	fs.Int64Var(&o.amount, "amount", 50000, "fee / money-in: amount in whole rupiah")
	fs.StringVar(&o.reff, "reff", "", "money-in: merchant reference (defaults to SMOKE-<timestamp>)")
	fs.StringVar(&o.bankVA, "va-bank", "BRI", "va: issuing bank (BCA, BNI, BRI, MANDIRI, …)")
	fs.IntVar(&o.timeoutSecs, "timeout", 30, "per-request timeout in seconds")
	fs.StringVar(&o.baseURL, "base-url", "", "override the host entirely (for testing against a stub)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Built by hand rather than through ConfigFromEnv so -production and -base-url can
	// override the environment. The names are trimmed the same way ConfigFromEnv trims
	// them: a trailing newline from a secrets mount lands inside the HMAC key and fails
	// every signature with nothing pointing at the cause.
	cfg := singapay.Config{
		ClientID:     strings.TrimSpace(os.Getenv(singapay.EnvClientID)),
		ClientSecret: strings.TrimSpace(os.Getenv(singapay.EnvClientSecret)),
		PartnerID:    strings.TrimSpace(os.Getenv(singapay.EnvPartnerID)),
		WebhookKey:   strings.TrimSpace(os.Getenv(singapay.EnvWebhookKey)),
		IsProduction: o.production,
		BaseURL:      o.baseURL,
	}
	for name, value := range map[string]string{
		singapay.EnvClientID:     cfg.ClientID,
		singapay.EnvClientSecret: cfg.ClientSecret,
		singapay.EnvPartnerID:    cfg.PartnerID,
	} {
		if value == "" {
			return fmt.Errorf("%s is not set", name)
		}
	}

	switch o.timestamp {
	case "unix":
		cfg.Timestamp = singapay.UnixSecondsTimestamp
	case "iso":
		cfg.Timestamp = singapay.ISO8601Timestamp
	default:
		return fmt.Errorf("-timestamp must be unix or iso, got %q", o.timestamp)
	}

	client, err := singapay.New(cfg)
	if err != nil {
		return err
	}

	env := "SANDBOX"
	if o.production {
		env = "PRODUCTION"
	}
	if o.baseURL != "" {
		env = o.baseURL
	}
	fmt.Printf("environment : %s\n", env)
	fmt.Printf("partner id  : %s\n", redact(cfg.PartnerID))
	fmt.Printf("client id   : %s\n", redact(cfg.ClientID))
	fmt.Printf("step        : %s\n\n", o.step)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(o.timeoutSecs)*time.Second)
	defer cancel()

	switch o.step {
	case "readonly":
		return readonly(ctx, client)
	case "token":
		return stepToken(ctx, client)
	case "balance":
		return stepBalance(ctx, client)
	case "accounts":
		return stepAccounts(ctx, client)
	case "methods":
		return stepMethods(ctx, client)
	case "payment-link":
		return stepPaymentLink(ctx, client, o)
	case "va":
		return stepVA(ctx, client, o)
	case "qris":
		return stepQRIS(ctx, client, o)
	case "signature":
		return stepSignature(ctx, cfg, o)
	case "beneficiary":
		return stepBeneficiary(ctx, client, o)
	case "fee":
		return stepFee(ctx, client, o)
	case "create-account":
		return stepCreateAccount(ctx, client, o)
	case "verify-webhook":
		return stepVerifyWebhook(cfg, o)
	default:
		return fmt.Errorf("unknown step %q", o.step)
	}
}

// readonly runs everything that cannot change state, in dependency order: a failure early
// on explains every failure after it.
func readonly(ctx context.Context, c *singapay.Client) error {
	steps := []struct {
		name string
		fn   func(context.Context, *singapay.Client) error
	}{
		{"access token", stepToken},
		{"merchant balance", stepBalance},
		{"list accounts", stepAccounts},
		{"payment methods", stepMethods},
	}
	for _, s := range steps {
		if err := s.fn(ctx, c); err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		fmt.Println()
	}
	fmt.Println("✔ read-only battery passed. Run -step signature next — that is the one")
	fmt.Println("  that settles which X-Timestamp format the signing middleware accepts.")
	return nil
}

func stepToken(ctx context.Context, c *singapay.Client) error {
	token, err := c.AccessToken(ctx)
	if err != nil {
		return explain(err)
	}
	fmt.Printf("✔ access token obtained (%s), the date-based signature is correct\n", redact(token))
	return nil
}

func stepBalance(ctx context.Context, c *singapay.Client) error {
	b, err := c.GetMerchantBalance(ctx)
	if err != nil {
		return explain(err)
	}
	fmt.Println("✔ merchant balance")
	fmt.Printf("    available : %s %s\n", b.Available.Currency, b.Available)
	fmt.Printf("    pending   : %s %s\n", b.Pending.Currency, b.Pending)
	fmt.Printf("    held      : %s %s\n", b.Held.Currency, b.Held)
	fmt.Printf("    total     : %s %s\n", b.Total.Currency, b.Total)
	// Held has no counterpart in the ledger; whether it is already inside Total is an
	// open question, and this is where a real answer becomes visible.
	if !b.Held.IsZero() {
		fmt.Println("    ⚠ held_balance is non-zero — confirm with Singapay whether it is part of total")
	}
	return nil
}

func stepAccounts(ctx context.Context, c *singapay.Client) error {
	accounts, page, err := c.ListAccounts(ctx)
	if err != nil {
		return explain(err)
	}
	fmt.Printf("✔ %d sub-account(s), page %d/%d\n", len(accounts), page.CurrentPage, page.TotalPages)
	for i, a := range accounts {
		if i >= 10 {
			fmt.Printf("    … and %d more\n", len(accounts)-10)
			break
		}
		fmt.Printf("    %-28s %-10s %-18s number=%s\n", a.ID, a.Status, a.Type, orDash(a.Number))
		// An account with no number cannot receive a platform-fee transfer: the
		// account-transfer endpoint names its beneficiary by number and nothing else.
		if a.Number == "" {
			fmt.Println("      ⚠ no account_number — cannot be the beneficiary of an account transfer")
		}
	}
	return nil
}

// stepSignature settles the X-Timestamp question.
//
// Every signed endpoint is money-out, so there is no read-only way to test the scheme.
// Instead this sends a disbursement that cannot possibly succeed — a bogus account id and
// a zero amount — and reads which error comes back. The signature middleware runs before
// the controller, so:
//
//	SP016            → the signature itself was rejected: wrong format
//	anything else    → the signature was accepted and the request reached validation
//
// Nothing can move: the account does not exist and the amount is below any minimum.
func stepSignature(ctx context.Context, base singapay.Config, o options) error {
	const bogusAccount = "00000000000000000000000000"

	fmt.Println("probing both documented X-Timestamp formats with a deliberately")
	fmt.Println("invalid disbursement (bogus account, zero amount — nothing can move)")
	fmt.Println()

	formats := []struct {
		label string
		fn    singapay.TimestampFunc
	}{
		{"unix seconds  (signing guide)", singapay.UnixSecondsTimestamp},
		{"ISO-8601      (OpenAPI spec)", singapay.ISO8601Timestamp},
	}

	var accepted []string
	for _, f := range formats {
		cfg := base
		cfg.Timestamp = f.fn
		c, err := singapay.New(cfg)
		if err != nil {
			return err
		}

		_, err = c.Disburse(ctx, singapay.DisburseRequest{
			AccountID:         bogusAccount,
			ReferenceNumber:   fmt.Sprintf("SMOKE-%d", time.Now().UnixNano()),
			BankCode:          "002",
			BankAccountNumber: "0000000000",
			Amount:            0,
			Notes:             "signature probe",
		})

		e, _ := singapay.AsError(err)
		switch {
		case err == nil:
			// Should be unreachable, but must never be read as a pass.
			fmt.Printf("  %-32s ⚠ succeeded unexpectedly — investigate before proceeding\n", f.label)
		case e != nil && e.Code == singapay.CodeSignatureInvalid:
			fmt.Printf("  %-32s ✘ SP016 — signature rejected\n", f.label)
		case e != nil && e.Code != "":
			fmt.Printf("  %-32s ✔ signature accepted (reached validation: %s %s)\n", f.label, e.Code, e.Message)
			accepted = append(accepted, f.label)
		default:
			fmt.Printf("  %-32s ? inconclusive: %v\n", f.label, err)
		}
	}

	fmt.Println()
	switch len(accepted) {
	case 0:
		return errors.New("neither format was accepted — check the client secret, the IP allowlist (SP017), and that the signed path matches exactly")
	case 1:
		fmt.Printf("✔ settled: %s\n", strings.TrimSpace(accepted[0]))
		fmt.Println("  Pin it in Config.Timestamp and record it in docs/105-singapay-migration.md §9.4.")
	default:
		fmt.Println("✔ both formats accepted — the middleware is lenient. Keep the default")
		fmt.Println("  (unix seconds) and note that §9.4 is moot.")
	}
	return nil
}

func stepBeneficiary(ctx context.Context, c *singapay.Client, o options) error {
	if o.bank == "" || o.account == "" {
		return errors.New("-bank and -account are required for this step")
	}
	b, err := c.CheckBeneficiary(ctx, o.bank, o.account)
	if err != nil {
		return explain(err)
	}
	// An unknown account is a valid answer here, not an error.
	if b.IsValid() {
		fmt.Printf("✔ valid: %s at %s\n", b.AccountName, b.BankName)
	} else {
		fmt.Printf("✔ call succeeded; account reported invalid: %s\n", orDash(b.Message))
	}
	return nil
}

func stepFee(ctx context.Context, c *singapay.Client, o options) error {
	if o.accountID == "" || o.swift == "" {
		return errors.New("-account-id and -swift are required for this step")
	}
	q, err := c.CheckFee(ctx, o.accountID, o.swift, o.amount)
	if err != nil {
		return explain(err)
	}
	fmt.Printf("✔ quote for %s to %s\n", q.NetAmount, q.Beneficiary.FullName)
	fmt.Printf("    net (beneficiary receives) : %s\n", q.NetAmount)
	fmt.Printf("    transfer fee               : %s\n", q.TransferFee)
	fmt.Printf("    gross (account is debited) : %s\n", q.GrossAmount)
	fmt.Println()
	fmt.Println("  The gross is what a withdrawal must reserve. Reserving only the net")
	fmt.Println("  leaves the ledger short by the fee on every payout.")
	return nil
}

func stepCreateAccount(ctx context.Context, c *singapay.Client, o options) error {
	if o.name == "" {
		return errors.New("-name is required for this step")
	}
	fmt.Println("⚠ this step writes: it creates a sub-account, and Singapay offers no way")
	fmt.Println("  to delete one — only to deactivate it.")
	fmt.Println()

	a, err := c.CreateAccount(ctx, singapay.CreateAccountRequest{
		Name: o.name,
		Type: singapay.AccountTypeOwned,
	})
	if err != nil {
		return explain(err)
	}
	fmt.Printf("✔ created\n")
	fmt.Printf("    id             : %s\n", a.ID)
	fmt.Printf("    account_number : %s\n", orDash(a.Number))
	fmt.Printf("    status         : %s\n", a.Status)
	fmt.Printf("    type           : %s\n", a.Type)
	if !a.IsActive() {
		fmt.Println("    ⚠ not active — an owned account should be active on creation")
	}
	fmt.Println()
	fmt.Println("  Store BOTH id and account_number: transfers name their beneficiary by number.")
	return nil
}

func stepMethods(ctx context.Context, c *singapay.Client) error {
	methods, err := c.ListPaymentMethods(ctx)
	if err != nil {
		return explain(err)
	}
	fmt.Printf("✔ %d payment method(s) active\n", len(methods))
	for _, m := range methods {
		fmt.Printf("    %-18s %-8s %s\n", m.Code, m.Group, m.Name)
	}
	fmt.Println()
	fmt.Println("  These codes are what a fee table must key on — no other spelling")
	fmt.Println("  are not accepted anywhere in this API.")
	return nil
}

// reference returns the merchant reference to attach to a money-in instrument. It is the
// key everything else matches on, so the tool never creates one without it.
func (o options) reference() string {
	if o.reff != "" {
		return o.reff
	}
	return fmt.Sprintf("SMOKE-%d", time.Now().Unix())
}

func stepPaymentLink(ctx context.Context, c *singapay.Client, o options) error {
	if o.accountID == "" {
		return errors.New("-account-id is required for this step")
	}
	ref := o.reference()

	link, err := c.CreatePaymentLink(ctx, o.accountID, singapay.CreatePaymentLinkRequest{
		ReffNo:      ref,
		Type:        singapay.PaymentLinkTotal,
		TotalAmount: o.amount,
		ExpiredAt:   time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		return explain(err)
	}

	fmt.Printf("✔ payment link created\n")
	fmt.Printf("    reff_no : %s\n", link.ReffNo)
	fmt.Printf("    amount  : %s\n", link.TotalAmount)
	fmt.Printf("    pay at  : %s\n", link.PaymentURL)
	fmt.Println()
	fmt.Println("  Open that URL and pay it to see a webhook arrive. Note the webhook's")
	fmt.Println("  transaction.reff_no will be an ATTEMPT id — the reference above comes")
	fmt.Println("  back nested under payment.additional_info.payment_link.reff_no.")
	fmt.Println("  A payment link reports no per-transaction fee anywhere.")
	return nil
}

func stepVA(ctx context.Context, c *singapay.Client, o options) error {
	if o.accountID == "" {
		return errors.New("-account-id is required for this step")
	}
	ref := o.reference()
	expiry := time.Now().Add(24 * time.Hour)

	va, err := c.CreateVirtualAccount(ctx, o.accountID, singapay.CreateVirtualAccountRequest{
		BankCode:       singapay.VABank(strings.ToUpper(o.bankVA)),
		Kind:           singapay.VATemporary,
		AmountType:     singapay.VAClosed,
		Name:           "Smoke Test",
		MerchantReffNo: ref,
		Amount:         o.amount,
		MaxUsage:       1,
		ExpiredAt:      singapay.MillisTimestamp(expiry),
	})
	if err != nil {
		return explain(err)
	}

	fmt.Printf("✔ virtual account issued\n")
	fmt.Printf("    number  : %s (%s)\n", va.Number, va.Bank.ShortName)
	fmt.Printf("    code    : %s\n", va.Code)
	fmt.Printf("    amount  : %s\n", va.Amount)
	fmt.Printf("    reff_no : %s\n", va.MerchantReffNo)
	fmt.Println()
	fmt.Println("  Pay into that number in sandbox to see a webhook arrive. Unlike a")
	fmt.Println("  payment link, the VA webhook carries the actual channel fee.")
	return nil
}

func stepQRIS(ctx context.Context, c *singapay.Client, o options) error {
	if o.accountID == "" {
		return errors.New("-account-id is required for this step")
	}
	ref := o.reference()

	q, err := c.GenerateQRIS(ctx, o.accountID, singapay.GenerateQRISRequest{
		Amount:         o.amount,
		MerchantReffNo: ref,
		ExpiredAt:      time.Now().Add(time.Hour).Format(time.RFC3339),
	})
	if err != nil {
		return explain(err)
	}

	fmt.Printf("✔ QRIS generated\n")
	fmt.Printf("    reff_no : %s\n", q.MerchantReffNo)
	fmt.Printf("    amount  : %s\n", q.Amount)
	fmt.Printf("    qr_data : %s…\n", truncate(q.QRData, 48))
	if q.MDRPercentage > 0 {
		fmt.Printf("    mdr     : %.2f%% → %s\n", q.MDRPercentage, q.MDRCost)
		fmt.Println()
		fmt.Println("  That percentage is the rate Singapay actually applied — the one")
		fmt.Println("  channel where a fee table can be verified rather than trusted.")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// explain renders a Singapay failure with the part that decides what to do next.
func explain(err error) error {
	e, ok := singapay.AsError(err)
	if !ok {
		return err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%v", err)
	if e.Code != "" {
		fmt.Fprintf(&b, "\n  outcome: %s", e.Outcome())
	}
	for field, detail := range e.Fields {
		fmt.Fprintf(&b, "\n  field %s: %v", field, detail)
	}

	switch e.Code {
	case singapay.CodeUnauthorizedIP:
		b.WriteString("\n  → this machine's public IP is not on the merchant allowlist; add it in the dashboard")
	case singapay.CodeSignatureInvalid:
		b.WriteString("\n  → signature rejected: check the client secret and the X-Timestamp format (-timestamp iso)")
	case singapay.CodeUnauthorized:
		b.WriteString("\n  → token rejected: check the client id/secret pair and that they match this environment")
	case singapay.CodeMerchantNotFound:
		b.WriteString("\n  → the account id does not belong to this merchant")
	}
	return errors.New(b.String())
}

func redact(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// stepVerifyWebhook answers which secret Singapay actually signs callbacks with.
//
// The question exists because the API documentation describes a single client_secret used
// for everything, while the merchant dashboard also issues a separate HMAC validation key.
// Only one of them verifies a real delivery, and guessing has a bad failure mode: the wrong
// key produces a signature mismatch on every callback, which reads like a canonicalisation
// bug and sends you looking at JSON encoding instead of at credentials.
//
// So this does not guess. Capture one real delivery — body, X-Signature, X-Timestamp,
// Authorization — and replay it here. Each candidate key is tried against the untouched
// bytes and the one that matches is the answer.
//
// Nothing is sent anywhere: this is pure local computation against a captured payload.
func stepVerifyWebhook(base singapay.Config, o options) error {
	if o.webhookFile == "" {
		return fmt.Errorf("-webhook-body is required: point it at a file holding the raw delivery body")
	}
	if o.webhookSignature == "" {
		return fmt.Errorf("-webhook-signature is required: copy the X-Signature header from the delivery")
	}

	// Read, never re-encode. The signature covers the bytes Singapay sent, so a
	// round-trip through a JSON decoder would change what is hashed and make every
	// candidate fail for a reason that has nothing to do with the key.
	body, err := os.ReadFile(o.webhookFile)
	if err != nil {
		return fmt.Errorf("reading webhook body: %w", err)
	}

	fmt.Printf("endpoint    : %s\n", o.webhookEndpoint)
	fmt.Printf("body        : %d bytes from %s\n", len(body), o.webhookFile)
	fmt.Printf("timestamp   : %s\n\n", o.webhookTimestamp)

	candidates := []struct {
		label string
		key   string
	}{
		{"client secret (SINGAPAY_CLIENT_SECRET)", base.ClientSecret},
	}
	if base.WebhookKey != "" && base.WebhookKey != base.ClientSecret {
		candidates = append(candidates, struct {
			label string
			key   string
		}{"webhook key   (SINGAPAY_WEBHOOK_KEY)", base.WebhookKey})
	}

	req := singapay.WebhookRequest{
		Endpoint:      o.webhookEndpoint,
		Body:          body,
		Signature:     o.webhookSignature,
		Timestamp:     o.webhookTimestamp,
		Authorization: o.webhookAuth,
	}

	var matched []string
	for _, candidate := range candidates {
		if candidate.key == "" {
			fmt.Printf("  %-40s — not set, skipped\n", candidate.label)
			continue
		}

		cfg := base
		cfg.WebhookKey = candidate.key
		c, err := singapay.New(cfg)
		if err != nil {
			return err
		}

		if err := c.VerifyWebhook(req); err != nil {
			fmt.Printf("  %-40s ✘ mismatch\n", candidate.label)
			continue
		}
		fmt.Printf("  %-40s ✔ MATCHES\n", candidate.label)
		matched = append(matched, candidate.label)
	}

	fmt.Println()
	switch len(matched) {
	case 1:
		fmt.Printf("Singapay signs callbacks with the %s.\n", strings.TrimSpace(matched[0]))
		fmt.Println("Set SINGAPAY_WEBHOOK_KEY accordingly (leave it unset if that is the client secret).")
		return nil
	case 0:
		// Worth being explicit that a failure here is not necessarily the key. If the
		// scheme itself differs, no key will ever match, and hunting for a third secret
		// would be the wrong search.
		fmt.Println("Neither key verifies this delivery. Before looking for another secret, check that:")
		fmt.Println("  - the body is the raw bytes, not re-serialised by a proxy or an editor")
		fmt.Println("  - -webhook-endpoint is the path registered in the dashboard, not the rewritten one")
		fmt.Println("  - X-Timestamp and Authorization are copied exactly, including the Bearer prefix or its absence")
		fmt.Println("If all three hold, Singapay's callback signature scheme differs from its request")
		fmt.Println("scheme, and no choice of key will fix it — the scheme is what needs changing.")
		return fmt.Errorf("no candidate key verified the delivery")
	default:
		// Both keys matching means they are the same secret under two labels.
		fmt.Println("Both candidates verify, so they are the same secret — leave SINGAPAY_WEBHOOK_KEY unset.")
		return nil
	}
}
