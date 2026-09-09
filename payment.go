package ledger

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"time"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/Aturjadwal/singapay-ledger/singapay"
)

// GeneratePaymentRequest contains the parameters to generate a payment
type GeneratePaymentRequest struct {
	// Seller information
	SellerAccountID string `json:"seller_account_id"`

	// Buyer information
	BuyerAccountID string `json:"buyer_account_id"`
	BuyerName      string `json:"buyer_name"`
	BuyerEmail     string `json:"buyer_email"`

	// Product information
	ProductID   string         `json:"product_id"`
	ProductType string         `json:"product_type"` // PHOTO, FOLDER, SUBSCRIPTION, etc.
	SellerPrice int64          `json:"seller_price"` // Price set by seller
	Currency    string         `json:"currency"`     // IDR or USD
	Metadata    map[string]any `json:"metadata"`     // Product details (title, resolution, etc.)

	// Payment configuration
	// PaymentChannel is a Singapay channel code: "QRIS", "VA_BCA", "EWALLET_DANA", …
	// Leaving it empty issues a payment link and lets the payer choose — which costs
	// fee reconciliation, because a payment link reports no per-transaction fee
	// anywhere. Name the channel whenever it is known.
	PaymentChannel        string   `json:"payment_channel"`
	ExpiresIn             int64    `json:"expires_in"`              // Payment expiration in minutes (default: 60)
	FeeModel              FeeModel `json:"fee_model"`               // Who pays gateway fee (defaults to GATEWAY_ON_CUSTOMER)
	SkipPlatformFee       bool     `json:"skip_platform_fee"`       // When true, platform fee is not charged
	PlatformFeeMultiplier int      `json:"platform_fee_multiplier"` // Multiplies platform fee only (e.g. installment: N due terms → multiplier=N). The gateway fee is not affected.
	CustomerPhone         string   `json:"customer_phone"`          // Required by some e-wallet vendors (OVO push-to-pay among them)
}

// GeneratePaymentResponse contains the result of payment generation
type GeneratePaymentResponse struct {
	TransactionID string `json:"transaction_id"`
	InvoiceNumber string `json:"invoice_number"`
	PaymentURL    string `json:"payment_url"`
	PaymentCode   string `json:"payment_code,omitempty"` // VA number, or the QRIS payload to render
	ExpiresAt     int64  `json:"expires_at"`             // Unix timestamp
	// PaymentChannel echoes the channel actually used. It differs from the request when
	// none was named: the response says PAYMENT_LINK, and the payer picks from there.
	PaymentChannel string `json:"payment_channel"`

	// Fee breakdown for transparency
	SellerPrice     int64  `json:"seller_price"`
	SellerNetAmount int64  `json:"seller_net_amount"` // What seller actually receives
	PlatformFee     int64  `json:"platform_fee"`
	GatewayFee      int64  `json:"gateway_fee"`
	TotalCharged    int64  `json:"total_charged"`
	FeeModel        string `json:"fee_model"`
	Currency        string `json:"currency"`
}

// GeneratePayment creates a new payment for a product purchase.
// Flow: calculate fees → build ProductTransaction → issue the Singapay instrument → save both
func (c *LedgerClient) GeneratePayment(ctx context.Context, req *GeneratePaymentRequest) (*GeneratePaymentResponse, error) {
	// Validate required fields
	if err := c.validateGeneratePaymentRequest(req); err != nil {
		return nil, err
	}

	// Get the seller's ledger account to obtain its Singapay sub-account ULID
	sellerAcccount, err := c.repoProvider.Account().GetBySellerID(ctx, req.SellerAccountID)
	if err != nil {
		if ledgererr.IsAppError(err, repo.ErrNotFound) {
			return nil, ledgererr.ErrLedgerNotFound.WithError(fmt.Errorf("seller account not found for seller_id: %s", req.SellerAccountID))
		}
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get seller account", err)
	}
	if sellerAcccount.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			fmt.Sprintf("seller account %s has no Singapay sub-account id; nothing can be charged into it", req.SellerAccountID), nil)
	}

	// Load fee configurations
	feeConfigs, err := c.repoProvider.FeeConfig().GetAllActive(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to load fee configurations", err)
	}

	// Calculate fees
	feeCalc := domain.NewFeeCalculator(feeConfigs)

	// Validate payment channel is supported (only when specified)
	if req.PaymentChannel != "" && !feeCalc.HasPaymentChannel(req.PaymentChannel) {
		return nil, ledgererr.ErrUnsupportedPaymentChannel.WithError(
			fmt.Errorf("payment channel %q not found in fee configs, supported: %v", req.PaymentChannel, feeCalc.SupportedPaymentChannels()),
		)
	}

	// Default to GATEWAY_ON_CUSTOMER if not specified (backward compatibility)
	feeModel := req.FeeModel
	if feeModel == "" {
		feeModel = domain.FeeModelGatewayOnCustomer
	}

	currency := domain.Currency(req.Currency)
	feeBreakdown := feeCalc.GetFeeBreakdownWithOptions(req.SellerPrice, req.PaymentChannel, currency, domain.FeeBreakdownOptions{
		FeeModel:              feeModel,
		SkipPlatformFee:       req.SkipPlatformFee,
		PlatformFeeMultiplier: req.PlatformFeeMultiplier,
	})

	c.logger.InfoContext(ctx, "Calculated fee breakdown",
		"seller_price", feeBreakdown.SellerPrice,
		"platform_fee", feeBreakdown.PlatformFee,
		"gateway_fee", feeBreakdown.GatewayFee,
		"total_charged", feeBreakdown.TotalCharged,
		"seller_net_amount", feeBreakdown.SellerNetAmount,
		"fee_model", feeBreakdown.FeeModel,
		"currency", feeBreakdown.Currency,
	)

	// Generate invoice number
	invoiceNumber := generateInvoiceNumber()

	c.logger.InfoContext(ctx, "Generated invoice number", "invoice_number", invoiceNumber)

	// Calculate expiration time
	// Payment due date is in minutes (default: 60 minutes, max: 999999)
	expiresInMinutes := req.ExpiresIn
	if expiresInMinutes <= 0 {
		expiresInMinutes = 60 // Default: 60 minutes
	}
	expiresAt := time.Now().Add(time.Duration(expiresInMinutes) * time.Minute)

	productTx := domain.NewProductTransaction(
		req.BuyerAccountID,
		sellerAcccount.UUID,
		req.ProductID,
		req.ProductType,
		invoiceNumber,
		feeBreakdown,
		req.Metadata,
	)

	// Issue the payment instrument at Singapay. Which product that is depends on the
	// channel; see issuePaymentInstrument.
	instrument, err := c.issuePaymentInstrument(ctx, paymentInstrumentRequest{
		AccountID:     sellerAcccount.SingapayAccountID,
		Channel:       req.PaymentChannel,
		Amount:        feeBreakdown.TotalCharged,
		InvoiceNumber: invoiceNumber,
		CustomerName:  req.BuyerName,
		CustomerEmail: req.BuyerEmail,
		CustomerPhone: req.CustomerPhone,
		Description:   req.ProductType,
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		c.logger.ErrorContext(ctx, "Singapay payment creation failed",
			"invoice_number", invoiceNumber,
			"payment_channel", req.PaymentChannel,
			"error", err,
		)
		return nil, err
	}

	// Create PaymentRequest
	paymentReq := domain.NewPaymentRequest(
		productTx.UUID,
		instrument.GatewayID,
		instrument.Channel,
		feeBreakdown.TotalCharged,
		currency,
		expiresAt,
	)
	paymentReq.SetPaymentURL(instrument.PaymentURL)
	if instrument.PaymentCode != "" {
		paymentReq.SetPaymentCode(instrument.PaymentCode)
	}

	// Save both ProductTransaction and PaymentRequest in a transaction
	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		// Save ProductTransaction
		if err := tx.ProductTransaction().Save(ctx, productTx); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save product transaction", err)
		}

		// Save PaymentRequest
		if err := tx.PaymentRequest().Save(ctx, paymentReq); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save payment request", err)
		}

		return nil
	})

	if err != nil {
		c.logger.ErrorContext(ctx, "Failed to save payment records",
			"invoice_number", invoiceNumber,
			"error", err,
		)
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to save payment records", err)
	}

	c.logger.InfoContext(ctx, "Payment generated successfully",
		"transaction_id", productTx.UUID,
		"invoice_number", invoiceNumber,
		"total_charged", feeBreakdown.TotalCharged,
		"payment_channel", instrument.Channel,
		"checkout_url", instrument.PaymentURL,
	)

	return &GeneratePaymentResponse{
		TransactionID:   productTx.UUID,
		InvoiceNumber:   invoiceNumber,
		PaymentURL:      instrument.PaymentURL,
		PaymentCode:     instrument.PaymentCode,
		PaymentChannel:  instrument.Channel,
		ExpiresAt:       expiresAt.Unix(),
		SellerPrice:     feeBreakdown.SellerPrice,
		SellerNetAmount: feeBreakdown.SellerNetAmount,
		PlatformFee:     feeBreakdown.PlatformFee,
		GatewayFee:      feeBreakdown.GatewayFee,
		TotalCharged:    feeBreakdown.TotalCharged,
		FeeModel:        string(feeBreakdown.FeeModel),
		Currency:        string(currency),
	}, nil
}

// GenerateSubscriptionPaymentRequest contains the parameters to generate a subscription payment
// where the beneficiary is the PLATFORM (no seller split).
type GenerateSubscriptionPaymentRequest struct {
	// Buyer information
	BuyerAccountID string `json:"buyer_account_id"`
	BuyerName      string `json:"buyer_name"`
	BuyerEmail     string `json:"buyer_email"`

	// Subscription information
	ProductID         string         `json:"product_id"`
	SubscriptionPrice int64          `json:"subscription_price"` // Price charged to buyer
	Currency          string         `json:"currency"`           // IDR or USD
	Metadata          map[string]any `json:"metadata"`

	// Payment configuration
	ExpiresIn int64 `json:"expires_in"` // Payment expiration in minutes (default: 60)
	// FeeModel is always GATEWAY_ON_SELLER: the customer pays subscription_price only and
	// the platform absorbs the gateway fee.
	// PaymentChannel is not required — the buyer selects one on the Singapay payment link.
}

// GenerateSubscriptionPayment creates a payment for a platform subscription purchase.
// Unlike GeneratePayment, there is no seller — the platform receives all net proceeds, and
// the payment is issued against the platform's own Singapay sub-account.
func (c *LedgerClient) GenerateSubscriptionPayment(ctx context.Context, req *GenerateSubscriptionPaymentRequest) (*GeneratePaymentResponse, error) {
	if err := c.validateGenerateSubscriptionPaymentRequest(req); err != nil {
		return nil, err
	}

	// Platform is the beneficiary — the payment is issued against its sub-account
	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get platform account", err)
	}
	if platformAccount.SingapayAccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInternal,
			"platform account has no Singapay sub-account id; it is provisioned once per environment by hand", nil)
	}

	// Load fee configurations
	feeConfigs, err := c.repoProvider.FeeConfig().GetAllActive(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to load fee configurations", err)
	}

	feeCalc := domain.NewFeeCalculator(feeConfigs)

	currency := domain.Currency(req.Currency)

	// GATEWAY_ON_SELLER: the customer pays subscription_price only and the platform
	// absorbs the gateway fee.
	// SkipPlatformFee=true: the platform IS the beneficiary, so there is no commission split.
	// PaymentChannel is empty — the buyer picks one on the payment link, so the gateway fee
	// is not known up front. It is also never learned: a payment link reports no
	// per-transaction fee, so a subscription's fee delta is always zero by construction.
	feeBreakdown := feeCalc.GetFeeBreakdownWithOptions(req.SubscriptionPrice, "", currency, domain.FeeBreakdownOptions{
		FeeModel:        domain.FeeModelGatewayOnSeller,
		SkipPlatformFee: true,
	})

	c.logger.InfoContext(ctx, "Calculated subscription fee breakdown",
		"subscription_price", feeBreakdown.SellerPrice,
		"gateway_fee", feeBreakdown.GatewayFee,
		"total_charged", feeBreakdown.TotalCharged,
		"platform_net_amount", feeBreakdown.SellerNetAmount,
		"fee_model", feeBreakdown.FeeModel,
		"currency", feeBreakdown.Currency,
	)

	invoiceNumber := generateInvoiceNumber()

	expiresInMinutes := req.ExpiresIn
	if expiresInMinutes <= 0 {
		expiresInMinutes = 60
	}
	expiresAt := time.Now().Add(time.Duration(expiresInMinutes) * time.Minute)

	// Platform account UUID acts as the "seller" so that HandlePaymentSuccess credits it correctly
	productTx := domain.NewProductTransaction(
		req.BuyerAccountID,
		platformAccount.UUID,
		req.ProductID,
		"SUBSCRIPTION",
		invoiceNumber,
		feeBreakdown,
		req.Metadata,
	)

	instrument, err := c.issuePaymentInstrument(ctx, paymentInstrumentRequest{
		AccountID:     platformAccount.SingapayAccountID,
		Channel:       "", // no channel pinned — the buyer picks one on the hosted page
		Amount:        feeBreakdown.TotalCharged,
		InvoiceNumber: invoiceNumber,
		CustomerName:  req.BuyerName,
		CustomerEmail: req.BuyerEmail,
		Description:   "SUBSCRIPTION",
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		c.logger.ErrorContext(ctx, "Singapay subscription payment creation failed",
			"invoice_number", invoiceNumber,
			"error", err,
		)
		return nil, err
	}

	paymentReq := domain.NewPaymentRequest(
		productTx.UUID,
		instrument.GatewayID,
		instrument.Channel,
		feeBreakdown.TotalCharged,
		currency,
		expiresAt,
	)
	paymentReq.SetPaymentURL(instrument.PaymentURL)
	if instrument.PaymentCode != "" {
		paymentReq.SetPaymentCode(instrument.PaymentCode)
	}

	err = c.txProvider.Transact(ctx, func(tx repo.Tx) error {
		if err := tx.ProductTransaction().Save(ctx, productTx); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save product transaction", err)
		}
		if err := tx.PaymentRequest().Save(ctx, paymentReq); err != nil {
			return ledgererr.NewError(ledgererr.CodeDatabaseError, "failed to save payment request", err)
		}
		return nil
	})

	if err != nil {
		c.logger.ErrorContext(ctx, "Failed to save subscription payment records",
			"invoice_number", invoiceNumber,
			"error", err,
		)
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to save subscription payment records", err)
	}

	c.logger.InfoContext(ctx, "Subscription payment generated successfully",
		"transaction_id", productTx.UUID,
		"invoice_number", invoiceNumber,
		"total_charged", feeBreakdown.TotalCharged,
		"checkout_url", instrument.PaymentURL,
	)

	return &GeneratePaymentResponse{
		TransactionID:   productTx.UUID,
		InvoiceNumber:   invoiceNumber,
		PaymentURL:      instrument.PaymentURL,
		PaymentCode:     instrument.PaymentCode,
		PaymentChannel:  instrument.Channel,
		ExpiresAt:       expiresAt.Unix(),
		SellerPrice:     feeBreakdown.SellerPrice,
		SellerNetAmount: feeBreakdown.SellerNetAmount,
		PlatformFee:     feeBreakdown.PlatformFee,
		GatewayFee:      feeBreakdown.GatewayFee,
		TotalCharged:    feeBreakdown.TotalCharged,
		FeeModel:        string(feeBreakdown.FeeModel),
		Currency:        string(currency),
	}, nil
}

// GeneratePaymentGatewayOnSeller creates a payment where seller absorbs the gateway fee
// Customer pays: seller_price + platform_fee (gateway fee NOT included)
// Seller receives: seller_price - gateway_fee (seller absorbs the gateway cost)
//
// Example: seller_price=10,000, platform=1,000, gateway=247
// → Customer pays: 11,000 (no gateway fee)
// → Seller receives: 9,753 (absorbed 247 gateway fee)
func (c *LedgerClient) GeneratePaymentGatewayOnSeller(ctx context.Context, req *GeneratePaymentRequest) (*GeneratePaymentResponse, error) {
	// Force fee model to GATEWAY_ON_SELLER
	req.FeeModel = FeeModelGatewayOnSeller
	return c.GeneratePayment(ctx, req)
}

// GeneratePaymentGatewayOnCustomer creates a payment where customer pays all fees (default behavior)
// Customer pays: seller_price + platform_fee + gateway_fee (all fees included)
// Seller receives: seller_price (100% of their listed price)
//
// Example: seller_price=10,000, platform=1,000, gateway=247
// → Customer pays: 11,247 (includes all fees)
// → Seller receives: 10,000 (100% of price)
func (c *LedgerClient) GeneratePaymentGatewayOnCustomer(ctx context.Context, req *GeneratePaymentRequest) (*GeneratePaymentResponse, error) {
	// Force fee model to GATEWAY_ON_CUSTOMER
	req.FeeModel = FeeModelGatewayOnCustomer
	return c.GeneratePayment(ctx, req)
}

// paymentInstrumentRequest is what every money-in channel needs, before the differences.
type paymentInstrumentRequest struct {
	AccountID     string // Singapay sub-account ULID that will receive the funds
	Channel       string // Singapay channel code, or "" to issue a payment link
	Amount        int64  // Total charged, in whole rupiah
	InvoiceNumber string // Our invoice, sent as the merchant reference on every channel
	CustomerName  string
	CustomerEmail string
	CustomerPhone string
	Description   string
	ExpiresAt     time.Time
}

// paymentInstrument is the part of a Singapay money-in response the ledger keeps.
type paymentInstrument struct {
	// GatewayID is Singapay's own id for the instrument — a VA ULID, a numeric link or
	// QRIS id, an e-wallet id. Stored so a support question can be answered without
	// guessing which product issued it.
	GatewayID string
	// PaymentURL is where to send the payer. QRIS has none: it returns a payload to
	// render instead, in PaymentCode.
	PaymentURL string
	// PaymentCode is the VA number, or the QRIS string to draw.
	PaymentCode string
	// Channel is the channel actually used, which is PAYMENT_LINK when none was named.
	Channel string
}

// issuePaymentInstrument creates the payment at Singapay, choosing the product from the
// channel.
//
// The choice matters beyond checkout ergonomics. A virtual account, a QRIS charge and an
// e-wallet order each report the fee Singapay took on that transaction; a payment link
// reports no fee anywhere — not on the webhook, not on the history row, not on any
// endpoint. Fee reconciliation is built on that number, so pinning the channel is what
// keeps it possible. A payment link is issued only when the caller names no channel, which
// is a deliberate trade rather than a default.
func (c *LedgerClient) issuePaymentInstrument(ctx context.Context, req paymentInstrumentRequest) (*paymentInstrument, error) {
	if req.AccountID == "" {
		return nil, ledgererr.NewError(ledgererr.CodeInvalidRequest,
			"cannot issue a payment against an account with no Singapay sub-account id", nil)
	}

	switch paymentChannelKind(req.Channel) {
	case channelVirtualAccount:
		bank, err := vaBankFromChannel(req.Channel)
		if err != nil {
			return nil, ledgererr.ErrUnsupportedPaymentChannel.WithError(err)
		}
		// A single invoice is a temporary, closed, single-use account with an expiry.
		// Note the encoding: Singapay wants 13-digit Unix milliseconds as a *string*
		// here, where a payment link wants ISO 8601. The helpers keep that straight.
		va, gwErr := c.gateway.CreateVirtualAccount(ctx, req.AccountID, singapay.CreateVirtualAccountRequest{
			BankCode:       bank,
			Kind:           singapay.VATemporary,
			AmountType:     singapay.VAClosed,
			Name:           req.CustomerName,
			MerchantReffNo: req.InvoiceNumber,
			ExpiredAt:      singapay.MillisTimestamp(req.ExpiresAt),
			MaxUsage:       1,
			Amount:         req.Amount,
		})
		if gwErr != nil {
			return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "failed to create virtual account", gwErr)
		}
		return &paymentInstrument{
			GatewayID:   va.ID,
			PaymentCode: va.Number,
			Channel:     req.Channel,
		}, nil

	case channelQRIS:
		qr, gwErr := c.gateway.GenerateQRIS(ctx, req.AccountID, singapay.GenerateQRISRequest{
			Amount:         req.Amount,
			ExpiredAt:      singapay.ISO8601Timestamp(req.ExpiresAt),
			MerchantReffNo: req.InvoiceNumber,
		})
		if gwErr != nil {
			return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "failed to generate QRIS", gwErr)
		}
		// QRIS has no hosted page: QRData is the payload the caller renders as a code.
		return &paymentInstrument{
			GatewayID:   strconv.FormatInt(qr.ID, 10),
			PaymentCode: qr.QRData,
			Channel:     req.Channel,
		}, nil

	case channelEwallet:
		order, gwErr := c.gateway.CreateEwalletOrder(ctx, singapay.CreateEwalletOrderRequest{
			AccountID:      req.AccountID,
			Amount:         req.Amount,
			Vendor:         req.Channel,
			ExpiredAt:      singapay.ISO8601Timestamp(req.ExpiresAt),
			CustomerName:   req.CustomerName,
			CustomerEmail:  req.CustomerEmail,
			CustomerPhone:  req.CustomerPhone,
			MerchantReffNo: req.InvoiceNumber,
		})
		if gwErr != nil {
			return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "failed to create e-wallet order", gwErr)
		}
		return &paymentInstrument{
			GatewayID:  strconv.FormatInt(order.ID, 10),
			PaymentURL: order.CheckoutURL,
			Channel:    req.Channel,
		}, nil

	case channelPaymentLink:
		maxUsage := 1
		link, gwErr := c.gateway.CreatePaymentLink(ctx, req.AccountID, singapay.CreatePaymentLinkRequest{
			ReffNo:      req.InvoiceNumber,
			Description: req.Description,
			Type:        singapay.PaymentLinkTotal,
			TotalAmount: req.Amount,
			MaxUsage:    &maxUsage,
			// Absolute, not a lifetime in minutes: compute it before calling.
			ExpiredAt: singapay.ISO8601Timestamp(req.ExpiresAt),
			// Only accepted when the link resolves to single use, which it does here.
			CustomerName:  req.CustomerName,
			CustomerEmail: req.CustomerEmail,
			CustomerPhone: req.CustomerPhone,
		})
		if gwErr != nil {
			return nil, ledgererr.NewError(ledgererr.CodeGatewayAPIError, "failed to create payment link", gwErr)
		}
		return &paymentInstrument{
			GatewayID:  strconv.FormatInt(link.ID, 10),
			PaymentURL: link.PaymentURL,
			Channel:    ChannelPaymentLink,
		}, nil

	default:
		return nil, ledgererr.ErrUnsupportedPaymentChannel.WithError(
			fmt.Errorf("payment channel %q is not a Singapay money-in product; expected QRIS, VA_*, EWALLET_*, or empty for a payment link", req.Channel),
		)
	}
}

// CalculateFeesForCustomer returns the fee breakdown without creating a transaction.
// Useful for showing the buyer the total cost before purchase.
// Uses GATEWAY_ON_CUSTOMER model: customer pays seller_price + platform_fee + gateway_fee.
// The response also includes the payment channel with the lowest gateway fee for the same seller price.
//
// platformFeeMultiplier controls platform fee behaviour:
//   - 0 → skip platform fee entirely
//   - 1 → normal platform fee (no multiply)
//   - >1 → platform fee multiplied by this value (e.g. installment with 2 due terms → 2)
//
// The gateway fee is never multiplied regardless of the multiplier value.
func (c *LedgerClient) CalculateFeesForCustomer(ctx context.Context, sellerPrice int64, paymentChannel string, currency string, platformFeeMultiplier int) (*FeeCalculationResponse, error) {
	feeConfigs, err := c.repoProvider.FeeConfig().GetAllActive(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to load fee configurations", err)
	}

	feeCalc := domain.NewFeeCalculator(feeConfigs)
	cur := domain.Currency(currency)
	opts := domain.FeeBreakdownOptions{
		FeeModel:              domain.FeeModelGatewayOnCustomer,
		SkipPlatformFee:       platformFeeMultiplier == 0,
		PlatformFeeMultiplier: platformFeeMultiplier,
	}

	breakdown := feeCalc.GetFeeBreakdownWithOptions(sellerPrice, paymentChannel, cur, opts)

	cheapestChannel, cheapestBreakdown := feeCalc.GetCheapestChannel(sellerPrice, cur, opts)

	return &FeeCalculationResponse{
		FeeBreakdown: breakdown,
		CheapestPaymentChannel: CheapestChannelInfo{
			PaymentChannel: cheapestChannel,
			GatewayFee:     cheapestBreakdown.GatewayFee,
			TotalCharged:   cheapestBreakdown.TotalCharged,
		},
	}, nil
}

// GetPaymentChannelFeeConfigs returns all fee configurations excluding the PLATFORM config.
// Use this to present available payment channels and their fee details to end users.
func (c *LedgerClient) GetPaymentChannelFeeConfigs(ctx context.Context) ([]*domain.FeeConfig, error) {
	configs, err := c.repoProvider.FeeConfig().GetAllExcludingPlatform(ctx)
	if err != nil {
		return nil, ledgererr.NewError(ledgererr.CodeInternal, "failed to get payment channel fee configs", err)
	}
	return configs, nil
}

// validateGenerateSubscriptionPaymentRequest validates the subscription payment request fields
func (c *LedgerClient) validateGenerateSubscriptionPaymentRequest(req *GenerateSubscriptionPaymentRequest) error {
	if req.BuyerAccountID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_account_id is required", nil)
	}
	if req.BuyerName == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_name is required", nil)
	}
	if req.BuyerEmail == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_email is required", nil)
	}
	if req.ProductID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "product_id is required", nil)
	}
	if req.SubscriptionPrice <= 0 {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "subscription_price must be positive", nil)
	}
	if req.Currency == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "currency is required", nil)
	}
	if req.Currency != string(domain.CurrencyIDR) && req.Currency != string(domain.CurrencyUSD) {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "currency must be IDR or USD", nil)
	}
	return nil
}

// validateGeneratePaymentRequest validates the payment request fields
func (c *LedgerClient) validateGeneratePaymentRequest(req *GeneratePaymentRequest) error {
	if req.SellerAccountID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "seller_account_id is required", nil)
	}
	if req.BuyerAccountID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_account_id is required", nil)
	}
	if req.BuyerName == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_name is required", nil)
	}
	if req.BuyerEmail == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "buyer_email is required", nil)
	}
	if req.ProductID == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "product_id is required", nil)
	}
	if req.SellerPrice <= 0 {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "seller_price must be positive", nil)
	}
	if req.Currency == "" {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "currency is required", nil)
	}

	// Validate currency
	if req.Currency != string(domain.CurrencyIDR) && req.Currency != string(domain.CurrencyUSD) {
		return ledgererr.NewError(ledgererr.CodeInvalidRequest, "currency must be IDR or USD", nil)
	}

	return nil
}

// generateInvoiceNumber creates a unique invoice number
func generateInvoiceNumber() string {
	now := time.Now()
	return fmt.Sprintf("INV-%s-%s", now.Format("20060102150405"), randomString(6))
}

func randomString(n int) string {
	return rand.Text()[:n]
}
