package ledger

import (
	"context"

	"github.com/Aturjadwal/singapay-ledger/domain"
	"github.com/Aturjadwal/singapay-ledger/ledgererr"
	"github.com/Aturjadwal/singapay-ledger/repo"
	"github.com/google/uuid"
)

// SetupDummyData seeds a development database. platformEmail is no longer used: nothing
// in this package provisions a platform sub-account any more (see the note where
// CreatePlatformAccount used to live). The parameter is kept so existing callers still
// compile.
func (c *LedgerClient) SetupDummyData(platformEmail string, sellerEmail string) {
	// Setup Platform Account.
	//
	// A real environment gets its platform account provisioned at Singapay by hand, then
	// a hand-inserted row carrying both the ULID and the account number. Seeding cannot
	// do that, so it settles for a local row with no Singapay identifiers — enough for
	// the dummy ledger entries below to balance, and unable to move money:
	// ProcessPlatformFeeTransfer refuses outright when the platform account has no
	// account number, so a seeded database announces itself instead of quietly
	// transferring fees somewhere unintended.
	platformAccount, err := c.repoProvider.Account().GetPlatformAccount(context.Background())
	if err != nil {
		if !ledgererr.IsAppError(err, repo.ErrNotFound) {
			c.logger.ErrorContext(context.Background(), "Failed to check platform account: aborting dummy setup", "error", err)
			return
		}
		seeded := domain.NewPlatformAccount("", domain.OWNER_TYPE_PLATFORM, domain.CurrencyIDR)
		if txErr := c.txProvider.Transact(context.Background(), func(tx repo.Tx) error {
			return tx.Account().Save(context.Background(), &seeded)
		}); txErr != nil {
			c.logger.ErrorContext(context.Background(), "Failed to seed platform account: aborting dummy setup", "error", txErr)
			return
		}
		platformAccount = &seeded
		c.logger.InfoContext(context.Background(), "Seeded platform account without Singapay identifiers", "account_id", platformAccount.Record.UUID)
	} else {
		c.logger.InfoContext(context.Background(), "Platform account already exists", "account_id", platformAccount.Record.UUID)
	}

	// Setup the gateway expense account. It is bookkeeping only — it holds the gateway
	// fees this ledger has recognised so settlement can clear them — so it has no
	// Singapay sub-account behind it and needs none.
	var gatewayAccount domain.Account
	c.txProvider.Transact(context.Background(), func(tx repo.Tx) error {

		existingGatewayAcc, err := tx.Account().GetPaymentGatewayAccount(context.Background())
		if err != nil && err == repo.ErrNotFound {
			c.logger.InfoContext(context.Background(), "No existing gateway account found, creating new one...")
		} else if err != nil {
			c.logger.ErrorContext(context.Background(), "Failed to check existing gateway account: skipping...", "error", err)
			return nil
		} else {
			c.logger.InfoContext(context.Background(), "Gateway account already exists", "account_id", existingGatewayAcc.Record.UUID)
			gatewayAccount = *existingGatewayAcc
			return nil
		}

		gatewayAccount = domain.NewPaymentGatewayAccount("", "SINGAPAY", domain.CurrencyIDR)
		err = tx.Account().Save(context.Background(), &gatewayAccount)
		if err != nil {
			c.logger.ErrorContext(context.Background(), "Failed to create gateway account: skipping...", "error", err)
		} else {
			c.logger.InfoContext(context.Background(), "Gateway account created", "account_id", gatewayAccount.Record.UUID)
		}

		return nil
	})

	// Creating dummy seller account
	sellerAccount, err := c.CreateAccount(context.Background(), sellerEmail, "Testing Name", domain.CurrencyIDR)
	if err != nil {
		c.logger.ErrorContext(context.Background(), "Failed to create seller account: skipping...", "error", err)
		sellerAccount, err = c.repoProvider.Account().GetBySellerID(context.Background(), sellerEmail)
		if err != nil {
			c.logger.ErrorContext(context.Background(), "Failed to get seller account by seller ID", "error", err)
			return
		}
	} else {
		c.logger.InfoContext(context.Background(), "Seller account created", "account_id", sellerAccount.Record.UUID)
	}

	dummyBuyerUUID := uuid.New().String()
	dummyProductID := uuid.New().String()

	// Transactions with COMPLETED status (paid by user, money in pending_balance)
	dummyCompletedTransactions := []map[string]any{
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          175000,
			"invoice_number": "INV-002-001",
			"metadata": map[string]any{
				"title":     "Medan Culinary Tour",
				"full_name": "Olivia Davis",
				"type":      "Folder",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          225000,
			"invoice_number": "INV-002-002",
			"metadata": map[string]any{
				"title":     "Semarang Historical Sites",
				"full_name": "James Wilson",
				"type":      "Photos",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          275000,
			"invoice_number": "INV-002-003",
			"metadata": map[string]any{
				"title":     "Makassar Beach Sunset",
				"full_name": "Isabella Martinez",
				"type":      "Folder",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          125000,
			"invoice_number": "INV-002-004",
			"metadata": map[string]any{
				"title":     "Balikpapan Nature Hike",
				"full_name": "William Anderson",
				"type":      "Photos",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          300000,
			"invoice_number": "INV-002-005",
			"metadata": map[string]any{
				"title":     "Pontianak River Cruise",
				"full_name": "Mia Thomas",
				"type":      "Folder",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          200000,
			"invoice_number": "INV-002-006",
			"metadata": map[string]any{
				"title":     "Manado Diving Experience",
				"full_name": "Benjamin Garcia",
				"type":      "Photos",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          180000,
			"invoice_number": "INV-002-007",
			"metadata": map[string]any{
				"title":     "Padang Culinary Delights",
				"full_name": "Charlotte Rodriguez",
				"type":      "Folder",
			},
		},
	}

	// Transactions with SETTLED status (money already in available_balance)
	dummySettledTransactions := []map[string]any{
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          100000,
			"invoice_number": "INV-003-001",
			"metadata": map[string]any{
				"title":     "Ngawi Lari Santai",
				"full_name": "Alice Johnson",
				"type":      "Photos",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          200000,
			"invoice_number": "INV-003-002",
			"metadata": map[string]any{
				"title":     "Jakarta Fun Run",
				"full_name": "Charlie Krik",
				"type":      "Folder",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          300000,
			"invoice_number": "INV-003-003",
			"metadata": map[string]any{
				"title":     "Bali Sunset Photos",
				"full_name": "Emily Carter",
				"type":      "Folder",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          312012,
			"invoice_number": "INV-003-004",
			"metadata": map[string]any{
				"title":     "Surabaya Marathon",
				"full_name": "David Lee",
				"type":      "Photos",
			},
		},
		{
			"buyer_id":       dummyBuyerUUID,
			"seller_id":      sellerAccount.Record.UUID,
			"product_id":     dummyProductID,
			"price":          150000,
			"invoice_number": "INV-003-005",
			"metadata": map[string]any{
				"title":     "Yogyakarta Street Food",
				"full_name": "Sophia Kim",
				"type":      "Folder",
			},
		},
	}

	// Dummy disbursements for seller withdrawals
	dummyDisbursements := []map[string]any{
		{
			"account_id":     sellerAccount.Record.UUID,
			"reference_id":   "DISB-001", // Unique reference for idempotency
			"amount":         400000,
			"bank_code":      "014", // BCA
			"account_number": "1234567890",
			"account_name":   "John Doe",
			"description":    "Withdrawal to BCA - January 2024",
		},
		{
			"account_id":     sellerAccount.Record.UUID,
			"reference_id":   "DISB-002",
			"amount":         350000,
			"bank_code":      "008", // Mandiri
			"account_number": "0987654321",
			"account_name":   "John Doe",
			"description":    "Withdrawal to Mandiri - February 2024",
		},
		{
			"account_id":     sellerAccount.Record.UUID,
			"reference_id":   "DISB-003",
			"amount":         250000,
			"bank_code":      "009", // BNI
			"account_number": "5555666677",
			"account_name":   "John Doe",
			"description":    "Withdrawal to BNI - March 2024",
		},
	}

	// Creating dummy transactions
	err = c.txProvider.Transact(context.Background(), func(tx repo.Tx) error {
		// Dummy transaction creation logic would go here
		// Load fee configurations
		feeConfigs, err := tx.FeeConfig().GetAllActive(context.Background())
		if err != nil {
			return ledgererr.NewError(ledgererr.CodeInternal, "failed to load fee configurations", err)
		}

		// Calculate fees
		feeCalc := domain.NewFeeCalculator(feeConfigs)

		// COMPLETED Transactions (paid, in pending_balance)
		for _, txData := range dummyCompletedTransactions {
			feeBreakdown := feeCalc.GetFeeBreakdown(int64(txData["price"].(int)), "QRIS", domain.CurrencyIDR)
			productTx := domain.NewProductTransaction(
				txData["buyer_id"].(string),
				txData["seller_id"].(string),
				txData["product_id"].(string),
				"PHOTO", // Default product type for dummy data
				txData["invoice_number"].(string),
				feeBreakdown,
				txData["metadata"].(map[string]any),
			)
			invoiceNum := txData["invoice_number"].(string)

			existingProductTx, err := tx.ProductTransaction().GetByInvoiceNumber(context.Background(), invoiceNum)
			if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
				c.logger.ErrorContext(context.Background(), "Failed to get existing product transaction", "invoice_number", invoiceNum, "error", err)
				return err
			}
			if existingProductTx != nil {
				c.logger.InfoContext(context.Background(), "Skipping existing product transaction", "invoice_number", invoiceNum)
				continue
			}

			// Create journal for payment success
			journal := domain.NewJournal(
				domain.EventTypePaymentSuccess,
				domain.SourceTypeProductTransaction,
				productTx.UUID,
				map[string]any{
					"invoice_number": invoiceNum,
					"seller_price":   productTx.Fee.SellerPrice,
					"platform_fee":   productTx.Fee.PlatformFee,
					"gateway_fee":    productTx.Fee.GatewayFee,
				},
			)

			ledgerEntries := domain.NewPaymentEntries(
				journal.UUID,
				productTx.UUID,
				productTx.SellerAccountID,
				productTx.Fee.SellerPrice,
				platformAccount.Record.UUID,
				productTx.Fee.PlatformFee,
				gatewayAccount.Record.UUID,
				productTx.Fee.GatewayFee,
			)

			productTx.MarkCompleted()

			// Save journal first
			if err := tx.Journal().Save(context.Background(), journal); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save journal", "error", err)
				return err
			}

			if err := tx.ProductTransaction().Save(context.Background(), productTx); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save product transaction", "error", err)
				return err
			}

			// Insert immutable ledger entries
			if err := tx.LedgerEntry().SaveBatch(context.Background(), ledgerEntries); err != nil {
				return err
			}

			// Here you would save the product transaction and ledger entries to the database
			// For this dummy setup, we're just generating the entries without persisting them

			c.logger.InfoContext(context.Background(), "Generated dummy transaction and ledger entries", "transaction_id", productTx.Record.UUID, "seller_amount", productTx.Fee.SellerPrice, "platform_fee", productTx.Fee.PlatformFee, "gateway_fee", productTx.Fee.GatewayFee)
		}

		// SETTLED Transactions (in available_balance)
		for _, txData := range dummySettledTransactions {
			feeBreakdown := feeCalc.GetFeeBreakdown(int64(txData["price"].(int)), "QRIS", domain.CurrencyIDR)
			productTx := domain.NewProductTransaction(
				txData["buyer_id"].(string),
				txData["seller_id"].(string),
				txData["product_id"].(string),
				"PHOTO", // Default product type for dummy data
				txData["invoice_number"].(string),
				feeBreakdown,
				txData["metadata"].(map[string]any),
			)
			invoiceNum := txData["invoice_number"].(string)

			existingProductTx, err := tx.ProductTransaction().GetByInvoiceNumber(context.Background(), invoiceNum)
			if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
				c.logger.ErrorContext(context.Background(), "Failed to get existing product transaction", "invoice_number", invoiceNum, "error", err)
				return err
			}
			if existingProductTx != nil {
				c.logger.InfoContext(context.Background(), "Skipping existing product transaction", "invoice_number", invoiceNum)
				continue
			}

			productTx.MarkCompleted()
			productTx.MarkSettled()
			if err := tx.ProductTransaction().Save(context.Background(), productTx); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save product transaction", "error", err)
				return err
			}

			// SETTLED transactions need BOTH payment entries AND settlement entries:
			// 1. Create payment journal and entries (add to PENDING when user paid)
			paymentJournal := domain.NewJournal(
				domain.EventTypePaymentSuccess,
				domain.SourceTypeProductTransaction,
				productTx.UUID,
				map[string]any{
					"invoice_number": invoiceNum,
					"seller_price":   productTx.Fee.SellerPrice,
					"platform_fee":   productTx.Fee.PlatformFee,
					"gateway_fee":    productTx.Fee.GatewayFee,
				},
			)

			paymentEntries := domain.NewPaymentEntries(
				paymentJournal.UUID,
				productTx.UUID,
				productTx.SellerAccountID,
				productTx.Fee.SellerPrice,
				platformAccount.Record.UUID,
				productTx.Fee.PlatformFee,
				gatewayAccount.Record.UUID,
				productTx.Fee.GatewayFee,
			)

			// 2. Create settlement journal and entries (move from PENDING to AVAILABLE).
			// Sourced on the transaction, the way the per-transaction settling pass
			// books it — there is no settlement batch to point at.
			settlementJournal := domain.NewJournal(
				domain.EventTypeSettlement,
				domain.SourceTypeProductTransaction,
				productTx.UUID,
				map[string]any{
					"invoice_number": invoiceNum,
				},
			)

			sellerEntry := domain.NewSettlementEntriesForAccount(
				settlementJournal.UUID,
				productTx.UUID,
				sellerAccount.Record.UUID,
				feeBreakdown.SellerPrice,
			)

			// Platform Fee Entry
			platformEntry := domain.NewSettlementEntriesForAccount(
				settlementJournal.UUID,
				productTx.UUID,
				platformAccount.Record.UUID,
				feeBreakdown.PlatformFee,
			)

			// Gateway fee entry
			gatewayEntry := domain.NewGatewayFeeSettlementEntry(
				settlementJournal.UUID,
				productTx.UUID,
				gatewayAccount.Record.UUID,
				feeBreakdown.GatewayFee,
			)

			// Save journals first
			if err := tx.Journal().Save(context.Background(), paymentJournal); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save payment journal", "error", err)
				return err
			}
			if err := tx.Journal().Save(context.Background(), settlementJournal); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save settlement journal", "error", err)
				return err
			}

			// Combine all ledger entries: payment + settlement
			settlementEntries := append(sellerEntry, platformEntry...)
			settlementEntries = append(settlementEntries, gatewayEntry)
			allEntries := append(paymentEntries, settlementEntries...)

			// Insert all ledger entries
			if err := tx.LedgerEntry().SaveBatch(context.Background(), allEntries); err != nil {
				return err
			}

			// Increment seller's total_deposit_amount since transaction is SETTLED
			if err := tx.Account().IncrementDeposit(context.Background(), productTx.SellerAccountID, productTx.Fee.SellerNetAmount); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to increment seller deposit amount", "error", err)
				return err
			}

			c.logger.InfoContext(context.Background(), "Generated dummy settled transaction and ledger entries", "transaction_id", productTx.Record.UUID, "seller_amount", productTx.Fee.SellerPrice, "platform_fee", productTx.Fee.PlatformFee, "gateway_fee", productTx.Fee.GatewayFee)
		}

		// Process disbursements (withdrawals)
		for _, disbData := range dummyDisbursements {
			accountID := disbData["account_id"].(string)
			referenceID := disbData["reference_id"].(string)
			amount := int64(disbData["amount"].(int))
			bankAccount := domain.BankAccount{
				BankCode:      disbData["bank_code"].(string),
				AccountNumber: disbData["account_number"].(string),
				AccountName:   disbData["account_name"].(string),
			}
			description := disbData["description"].(string)

			// Check if disbursement already exists by looking for ledger entry with this reference
			existingEntries, err := tx.LedgerEntry().GetBySourceID(context.Background(), referenceID)
			if err != nil && !ledgererr.IsAppError(err, repo.ErrNotFound) {
				c.logger.ErrorContext(context.Background(), "Failed to check existing disbursement", "reference_id", referenceID, "error", err)
				return err
			}
			if len(existingEntries) > 0 {
				c.logger.InfoContext(context.Background(), "Skipping existing disbursement", "reference_id", referenceID)
				continue
			}

			// Create disbursement with reference ID as external transaction ID
			disbursement, err := domain.NewDisbursement(
				accountID,
				amount,
				domain.CurrencyIDR,
				bankAccount,
				description,
			)
			if err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to create disbursement", "error", err)
				return err
			}

			// Mark as completed for dummy data using reference ID
			if err := disbursement.MarkCompleted(referenceID); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to mark disbursement as completed", "error", err)
				return err
			}

			// Create journal for disbursement
			disbursementJournal := domain.NewJournal(
				domain.EventTypeDisbursement,
				domain.SourceTypeDisbursement,
				referenceID, // Use reference_id as source_id for idempotency
				map[string]any{
					"amount":      amount,
					"bank_code":   bankAccount.BankCode,
					"description": description,
				},
			)

			// Save journal first
			if err := tx.Journal().Save(context.Background(), disbursementJournal); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save disbursement journal", "error", err)
				return err
			}

			// Save disbursement
			if err := tx.Disbursement().Save(context.Background(), disbursement); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save disbursement", "error", err)
				return err
			}

			// Create ledger entry for withdrawal (debits available balance)
			// Use reference ID as source_id for idempotency checking
			withdrawalEntry := domain.NewDisbursementEntry(
				disbursementJournal.UUID,
				referenceID, // Use reference_id as source_id
				accountID,
				amount,
			)

			// Save ledger entry (this will automatically update account balances)
			if err := tx.LedgerEntry().Save(context.Background(), withdrawalEntry); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to save withdrawal entry", "error", err)
				return err
			}

			// Increment total_withdrawal_amount since disbursement is COMPLETED
			if err := tx.Account().IncrementWithdrawal(context.Background(), accountID, amount); err != nil {
				c.logger.ErrorContext(context.Background(), "Failed to increment withdrawal amount", "error", err)
				return err
			}

			c.logger.InfoContext(context.Background(), "Generated dummy disbursement",
				"disbursement_id", disbursement.Record.UUID,
				"amount", amount,
				"bank_code", bankAccount.BankCode)
		}

		return nil

		// feeBreakdown := feeCalc.GetFeeBreakdown(100000, "QRIS", domain.CurrencyIDR)
		// productTx := domain.NewProductTransaction(
		// 	"dummy-buyer-account-id",
		// 	sellerEmail,
		// 	"123-456-789-1011",
		// 	generateInvoiceNumber(),
		// 	feeBreakdown,
		// 	map[string]any{
		// 		"title": "Dummy Product",
		// 	},
		// )

		// // PENDING Transactions
		// ledgerEntries := domain.NewPaymentEntries(
		// 	productTx.Record.UUID,
		// 	productTx.SellerAccountID,
		// 	productTx.Fee.SellerPrice,
		// 	platformAccount.Record.UUID,
		// 	productTx.Fee.PlatformFee,
		// 	gatewayAccount.Record.UUID,
		// 	productTx.Fee.GatewayFee,
		// )

		// return nil
	})

	if err != nil {
		c.logger.ErrorContext(context.Background(), "Failed to create dummy transactions: skipping...", "error", err)
	}
}
