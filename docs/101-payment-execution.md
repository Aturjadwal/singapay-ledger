# Payment Execution — Architecture Diagram

This diagram outlines the payment request lifecycle, from creation to completion, its
interaction with Singapay, and the initial ledger recording.

```mermaid
sequenceDiagram
    participant Payer
    participant Frontend
    participant LedgerAPI
    participant Singapay

    %% Step 1: Create the payment instrument
    Payer->>Frontend: Select product & pay
    Frontend->>LedgerAPI: GeneratePayment (channel, amount, invoice)
    Note right of LedgerAPI: The channel selects the product:<br/>QRIS / VA_* / EWALLET_* / payment link
    LedgerAPI->>Singapay: Create VA, QRIS, e-wallet order, or payment link
    Singapay-->>LedgerAPI: VA number, QR payload, or checkout URL
    LedgerAPI->>LedgerAPI: Save ProductTransaction (PENDING) + PaymentRequest
    Note right of LedgerAPI: Only the transaction carries a status;<br/>the payment request has none by design
    LedgerAPI-->>Frontend: Payment info

    %% Step 1b: The payer comes back later
    Payer->>Frontend: Reopen the payment (link in an email or message)
    Frontend->>LedgerAPI: GetPaymentByInvoiceNumber
    LedgerAPI-->>Frontend: Same instrument, plus Status and IsExpired
    Note right of LedgerAPI: PENDING and not expired → show it again.<br/>Anything else → do not issue a second payment<br/>without saying why

    %% Step 2: Payment confirmation
    Payer->>Singapay: Complete payment
    Singapay->>LedgerAPI: POST transaction_notif_url

    rect rgb(240, 240, 240)
    Note over LedgerAPI, Singapay: HandlePaymentSuccess
    LedgerAPI->>LedgerAPI: Verify HMAC-SHA512 signature FIRST
    Note right of LedgerAPI: Nothing in the body is read until it passes
    LedgerAPI->>LedgerAPI: Resolve the merchant reference to an invoice
    Note right of LedgerAPI: Every channel puts it somewhere different;<br/>a payment link's transaction.reff_no is an<br/>ATTEMPT id, not the reference we sent
    LedgerAPI->>LedgerAPI: Already booked? → no-op (Singapay retries)
    LedgerAPI->>LedgerAPI: ProductTransaction PENDING → COMPLETED (compare-and-set)
    end

    %% Step 3: Ledger recording
    Note right of LedgerAPI: Balances are NOT available yet.<br/>Waiting for settlement.
    LedgerAPI->>LedgerAPI: Create Journal (PAYMENT_SUCCESS)

    LedgerAPI->>LedgerAPI: LedgerEntry (Seller) → PENDING
    Note right of LedgerAPI: +SellerNetAmount

    LedgerAPI->>LedgerAPI: LedgerEntry (Platform) → PENDING
    Note right of LedgerAPI: +PlatformFee

    LedgerAPI->>LedgerAPI: LedgerEntry (Gateway expense) → PENDING
    Note right of LedgerAPI: +GatewayFee
```

## Key principles

- **ProductTransaction**: the business event (someone bought item X).
- **PaymentRequest**: the financial interaction (they paid Y through channel Z).
- **Ledger entries created**:
  - **Journal**: EventType `PAYMENT_SUCCESS`
  - **Seller entry**: `+SellerNetAmount` into **PENDING**
  - **Platform entry**: `+PlatformFee` into **PENDING**
  - **Gateway entry**: `+GatewayFee` into **PENDING**
- **Gateway identifiers recorded**: the webhook is the first moment the PAYMENT exists at
  Singapay for every channel, so `SetGatewayTransaction` stores both
  `gateway_transaction_id` (numeric) and `gateway_transaction_ref` (business id) on the
  payment request, in the same database transaction as the status move. Settlement reads the
  transaction back with them — see [102](./102-settlement-reconciliation.md).
- **Why pending?** Singapay holds the funds until settlement. Nothing is withdrawable yet.

## Three things about Singapay that shape this

**One callback URL carries four products.** VA, QRIS, e-wallet and payment link all arrive on
`transaction_notif_url` and are told apart by the envelope's `event` field — except that
Singapay's own payment-link sample carries no `event` field at all. Routing on `event` alone
would silently drop payment-link confirmations, so the parser falls back to the payment
method.

**The merchant reference lives somewhere different on every channel.** VA puts it in
`reff_no`; QRIS and e-wallet in `merchant_reff_no`; a payment link nests it on the payment
link object, because its transaction's own `reff_no` is the id of one payment *attempt*.
`MoneyInNotification.MerchantReference` resolves that. Reading the fields directly would
never match a payment link to its invoice.

**The instrument id is not the payment id.** `request_id`, recorded when the instrument was
created, names the instrument — and for VA and payment link that is a different entity from
the transaction: a VA is a container, and the payment arriving in it has its own business id;
a payment link can carry several attempts. So the webhook's identifiers are stored separately,
and settlement uses those. Two of them, because the four detail endpoints disagree about which
one they take.

**The amounts booked come from the transaction as priced, not from the webhook.** The fee
Singapay actually took is not final until settlement. If the webhook reports a charged amount
or a channel fee that disagrees with what was expected, both are logged and recorded in the
journal — but the entries are written from the transaction, so a disagreement cannot corrupt
the books before anyone has looked at it.
