# Ledger — Entity Reference

Singapaymen ini menjelaskan seluruh tabel/entitas yang terlibat dalam operasional ledger: struktur field, relasi antar entitas, lifecycle status, dan peran masing-masing dalam alur bisnis.

---

## Daftar Entitas

| Entitas | Tabel | Peran |
|---|---|---|
| [Account](#1-account) | `ledger_accounts` | Akun keuangan (seller, platform, payment gateway) |
| [ProductTransaction](#2-producttransaction) | `product_transactions` | Transaksi penjualan produk |
| [PaymentRequest](#3-paymentrequest) | `payment_requests` | Sesi pembayaran Singapay |
| [Journal](#4-journal) | `journals` | Pengelompokan event akuntansi |
| [LedgerEntry](#5-ledgerentry) | `ledger_entries` | Entri double-entry yang immutable |
| [FeeConfig](#6-feeconfig) | `fee_configs` | Konfigurasi fee platform dan Singapay |
| [SettlementBatch](#7-settlementbatch) | `settlement_batches` | Batch upload CSV settlement Singapay |
| [SettlementItem](#8-settlementitem) | `settlement_items` | Baris individual dari CSV settlement |
| [Disbursement](#9-disbursement) | `disbursements` | Penarikan saldo seller ke rekening bank |
| [ReconciliationDiscrepancy](#10-reconciliationdiscrepancy) | `reconciliation_discrepancies` | Selisih saldo yang terdeteksi saat rekonsiliasi |
| [Verification](#11-verification) | `ledger_verifications` | Verifikasi KYC seller |

---

## 1. Account

**Tabel:** `ledger_accounts`

Entitas inti yang merepresentasikan akun keuangan dalam sistem. Setiap seller memiliki satu akun, dan sistem memiliki satu akun PLATFORM dan satu akun PAYMENT_GATEWAY.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier internal akun |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `singapay_account_id` | VARCHAR(100) UNIQUE | ID sub-account Singapay yang ditautkan |
| `owner_type` | VARCHAR(20) | `SELLER`, `PLATFORM`, `PAYMENT_GATEWAY`, atau `RESERVE` |
| `owner_id` | VARCHAR(255) | Seller ID, `"PLATFORM"`, nama gateway, atau identifier reserve |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `pending_balance` | BIGINT | Cache saldo dana yang sudah di-capture tapi belum settled |
| `available_balance` | BIGINT | Cache saldo dana yang sudah settled dan bisa ditarik |
| `total_withdrawal_amount` | BIGINT | Total kumulatif penarikan melalui disbursement |
| `total_deposit_amount` | BIGINT | Total kumulatif deposit dari transaksi/settlement |
| `created_at` | TIMESTAMP | Waktu pembuatan akun |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Tipe Akun (`owner_type`)

- **`SELLER`** — Akun milik seller. Satu per seller. Saldo tumbuh dari transaksi yang settled.
- **`PLATFORM`** — Satu akun untuk platform. Menerima platform fee dari setiap transaksi.
- **`PAYMENT_GATEWAY`** — Satu akun untuk Singapay. Menerima gateway fee dari setiap transaksi.
- **`RESERVE`** — Akun cadangan untuk penyesuaian manual.

### Catatan Penting

- Saldo aktual **tidak disimpan langsung** di kolom `pending_balance`/`available_balance`. Nilai aslinya dihitung dengan menjumlahkan `ledger_entries`. Kolom cache diperbarui secara berkala untuk performa.
- Hanya ada **satu** akun PLATFORM dan **satu** akun PAYMENT_GATEWAY dalam seluruh sistem.

### Relasi

- `1:N` ke `product_transactions` (sebagai buyer maupun seller)
- `1:N` ke `disbursements`
- `1:N` ke `settlement_batches`
- `1:N` ke `ledger_entries`
- `1:1` ke `ledger_verifications`
- `1:N` ke `reconciliation_discrepancies`

---

## 2. ProductTransaction

**Tabel:** `product_transactions`

Entitas pusat yang merepresentasikan penjualan produk antara buyer dan seller. Ini adalah pemicu utama seluruh alur akuntansi.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier transaksi |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `buyer_account_id` | VARCHAR(255) | UUID akun buyer |
| `seller_account_id` | VARCHAR(255) | UUID akun seller |
| `product_id` | VARCHAR(255) | Identifier produk eksternal |
| `product_type` | VARCHAR(50) | `PHOTO`, `FOLDER`, `SUBSCRIPTION`, dsb. |
| `invoice_number` | VARCHAR(50) UNIQUE | Nomor invoice untuk pencocokan CSV settlement |
| `seller_price` | BIGINT | Harga yang ditetapkan seller |
| `platform_fee` | BIGINT | Markup platform di atas harga seller |
| `gateway_fee` | BIGINT | Fee payment gateway Singapay |
| `total_charged` | BIGINT | Total yang dibebankan ke buyer |
| `seller_net_amount` | BIGINT | Jumlah bersih yang diterima seller |
| `fee_model` | VARCHAR(50) | `GATEWAY_ON_CUSTOMER` atau `GATEWAY_ON_SELLER` |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `platform_fee_transferred` | BOOLEAN | Apakah platform fee sudah ditransfer ke sub-account platform |
| `platform_fee_transferred_at` | TIMESTAMP | Waktu transfer platform fee |
| `transfer_request_id` | TEXT | Singapay request-id untuk transfer platform fee (dipakai ulang saat retry untuk idempotency) |
| `completed_at` | TIMESTAMP | Waktu pembayaran dikonfirmasi webhook Singapay |
| `settled_at` | TIMESTAMP | Waktu muncul dalam CSV settlement |
| `metadata` | JSONB | Detail produk (photo_id, title, resolution, dll.) |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Fee Model

| Model | Siapa yang Bayar Gateway fee | Efek pada `total_charged` | Efek pada `seller_net_amount` |
|---|---|---|---|
| `GATEWAY_ON_CUSTOMER` | Buyer | `seller_price + platform_fee + gateway_fee` | `seller_price` (penuh) |
| `GATEWAY_ON_SELLER` | Seller | `seller_price + platform_fee` | `seller_price - gateway_fee` |

### Lifecycle Status

```
PENDING ──► COMPLETED ──► SETTLED
   │             │
   └─────────────┴──► FAILED

SETTLED ──► REFUNDED
```

- **`PENDING`** — Invoice dibuat, menunggu pembayaran.
- **`COMPLETED`** — money-in webhook mengkonfirmasi pembayaran. Ledger entry dibuat di sini.
- **`SETTLED`** — Transaksi muncul di CSV settlement Singapay.
- **`FAILED`** — Transaksi gagal di titik mana pun.
- **`REFUNDED`** — Dana dikembalikan ke buyer.

### Relasi

- `N:1` ke `ledger_accounts` (buyer dan seller)
- `1:1` ke `payment_requests`
- `1:N` ke `settlement_items`
- Direferensikan oleh `journals` (sebagai `source_type='PRODUCT_TRANSACTION'`)

---

## 3. PaymentRequest

**Tabel:** `payment_requests`

Melacak lifecycle integrasi dengan Singapay payment gateway. Satu `PaymentRequest` per `ProductTransaction`.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier request |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `product_transaction_uuid` | VARCHAR(255) FK | Transaksi yang ditautkan |
| `request_id` | VARCHAR(100) UNIQUE | ID payment request dari Singapay |
| `payment_code` | TEXT | Nomor VA, atau payload EMVCo QRIS lengkap yang di-scan pembeli. |
| `payment_channel` | VARCHAR(50) | `QRIS`, `VA_BCA`, `VA_BRI`, `VA_MANDIRI`, `VA_BNI`, `CREDIT_CARD`, `E_WALLET` |
| `payment_url` | TEXT | URL bagi buyer untuk menyelesaikan pembayaran |
| `amount` | BIGINT | Total yang dibebankan ke buyer |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `failure_reason` | TEXT | Detail error jika gagal |
| `completed_at` | TIMESTAMP | Waktu konfirmasi dari money-in webhook |
| `expires_at` | TIMESTAMP | Batas waktu kadaluarsa link pembayaran |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Lifecycle Status

```
PENDING ──► COMPLETED
   │
   ├──► FAILED
   └──► EXPIRED
```

- **`PENDING`** — Menunggu pembayaran dari buyer.
- **`COMPLETED`** — Singapay mengkonfirmasi pembayaran berhasil.
- **`FAILED`** — Pembayaran gagal.
- **`EXPIRED`** — Link pembayaran melewati `expires_at` (biasanya 24 jam).

`COMPLETED`, `FAILED`, dan `EXPIRED` adalah terminal state — tidak ada transisi lebih lanjut.

### Relasi

- `N:1` ke `product_transactions`

---

## 4. Journal

**Tabel:** `journals`

Event akuntansi atomik yang mengelompokkan satu atau lebih `ledger_entries`. Setiap event bisnis (pembayaran, settlement, penarikan) menghasilkan tepat satu journal.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier journal |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `event_type` | VARCHAR(50) | Lihat tipe event di bawah |
| `source_type` | VARCHAR(50) | `PRODUCT_TRANSACTION`, `SETTLEMENT_BATCH`, `DISBURSEMENT`, `MANUAL_ADJUSTMENT` |
| `source_id` | VARCHAR(255) | UUID entitas bisnis yang memicu journal ini |
| `metadata` | JSONB | Konteks tambahan event |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Tipe Event (`event_type`)

| Event | Pemicu | Entri yang Dihasilkan |
|---|---|---|
| `PAYMENT_SUCCESS` | money-in webhook konfirmasi pembayaran | 3 entri: seller (PENDING), platform (PENDING), Singapay (PENDING) |
| `SETTLEMENT` | Rekonsiliasi CSV settlement | 2 entri per akun: debit PENDING, kredit AVAILABLE |
| `DISBURSEMENT` | Seller membuat permintaan penarikan | 1 entri: debit AVAILABLE seller |
| `RECONCILIATION` | Penyesuaian selisih rekonsiliasi | Bervariasi |
| `MANUAL_ADJUSTMENT` | Koreksi manual oleh admin | Bervariasi |

### Catatan Penting

Journal bersifat **immutable** — tidak ada UPDATE atau DELETE. Ini menjaga audit trail yang lengkap dan tidak bisa dimanipulasi.

### Relasi

- `1:N` ke `ledger_entries`
- `N:1` ke entitas sumber (via `source_type` + `source_id`)

---

## 5. LedgerEntry

**Tabel:** `ledger_entries`

Catatan double-entry yang immutable. Setiap entri merepresentasikan debit atau kredit pada bucket saldo tertentu milik sebuah akun. Ini adalah fondasi dari seluruh sistem akuntansi.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier entri |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `journal_uuid` | VARCHAR(255) FK | Journal yang mengelompokkan entri ini |
| `account_uuid` | VARCHAR(255) FK | Akun yang terpengaruh |
| `amount` | BIGINT | Jumlah: positif = kredit, negatif = debit |
| `balance_bucket` | VARCHAR(10) | `PENDING` atau `AVAILABLE` |
| `balance_after` | BIGINT | Saldo running setelah entri ini (untuk query cepat) |
| `entry_type` | VARCHAR(50) | Lihat tipe entri di bawah |
| `source_type` | VARCHAR(50) | `PRODUCT_TRANSACTION`, `DISBURSEMENT`, `SETTLEMENT_BATCH`, `MANUAL_ADJUSTMENT` |
| `source_id` | VARCHAR(255) | UUID entitas bisnis asal |
| `metadata` | JSONB | Konteks tambahan |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Tipe Entri (`entry_type`)

| Tipe | Bucket | Arah | Keterangan |
|---|---|---|---|
| `PRODUCT_PAYMENT` | PENDING | Kredit | Dana masuk ke seller saat transaksi selesai |
| `PLATFORM_COMMISSION` | PENDING | Kredit | Fee platform masuk ke akun platform |
| `PROCESSOR_FEE` | PENDING | Kredit | gateway fee masuk ke akun PAYMENT_GATEWAY |
| `SETTLEMENT_CLEAR` | PENDING | Debit | Membersihkan PENDING saat settlement |
| `SETTLEMENT_NET` | AVAILABLE | Kredit | Memindahkan dana ke AVAILABLE saat settlement |
| `SETTLEMENT` | PENDING / AVAILABLE | Bervariasi | Entry settlement generik (legacy) |
| `DISBURSEMENT` | AVAILABLE | Debit | Penarikan dari saldo AVAILABLE |
| `RECONCILIATION` | AVAILABLE / PENDING | Bervariasi | Penyesuaian selisih |
| `FEE_ADJUSTMENT` | AVAILABLE / PENDING | Bervariasi | Penyesuaian fee (koreksi selisih fee aktual vs yang dihitung) |

### Dua Bucket Saldo

```
PENDING ──[settlement]──► AVAILABLE ──[disbursement]──► (rekening bank seller)
```

- **`PENDING`** — Dana yang sudah di-capture dari buyer tetapi belum dikonfirmasi dalam CSV settlement Singapay.
- **`AVAILABLE`** — Dana yang sudah dikonfirmasi settlement dan siap ditarik oleh seller.

### Set Entri per Event

**PAYMENT_SUCCESS** (3 entri):
1. `PRODUCT_PAYMENT` → akun seller, PENDING, kredit `seller_net_amount`
2. `PLATFORM_COMMISSION` → akun platform, PENDING, kredit `platform_fee`
3. `PROCESSOR_FEE` → akun Singapay, PENDING, kredit `gateway_fee`

**SETTLEMENT** (2 entri per akun seller):
1. `SETTLEMENT_CLEAR` → akun seller, PENDING, debit `seller_net_amount`
2. `SETTLEMENT_NET` → akun seller, AVAILABLE, kredit `seller_net_amount`

### Catatan Penting

`ledger_entries` bersifat **insert-only** — tidak pernah diupdate atau didelete. Saldo dihitung dengan menjumlahkan entri yang ada.

### Relasi

- `N:1` ke `journals`
- `N:1` ke `ledger_accounts`

---

## 6. FeeConfig

**Tabel:** `fee_configs`

Konfigurasi fee platform dan Singapay per payment channel. Digunakan oleh `FeeCalculator` untuk menghitung biaya setiap transaksi.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier config |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `config_type` | VARCHAR(20) | `PLATFORM` atau `Singapay` |
| `payment_channel` | VARCHAR(50) | Channel pembayaran (lihat di bawah) |
| `name` | VARCHAR(100) | Nama human-readable |
| `fee_type` | VARCHAR(20) | `FIXED` atau `PERCENTAGE` |
| `fixed_amount` | BIGINT | Fee tetap dalam satuan terkecil mata uang |
| `percentage` | DECIMAL(10,6) | Fee persentase (misal: `2.2` = 2,2%) |
| `is_active` | BOOLEAN | Apakah config ini aktif digunakan |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Tipe Fee (`fee_type`)

| Tipe | Cara Hitung |
|---|---|
| `FIXED` | Fee tetap: `fixed_amount` |
| `PERCENTAGE` | `total = base / (1 - percentage%)` |

### Konfigurasi Default

| Type | Channel | Fee |
|---|---|---|
| PLATFORM | — | Rp 1.000 (fixed) per transaksi |
| Singapay | QRIS | 2,2% (percentage) |
| Singapay | VIRTUAL_ACCOUNT | Rp 4.500 (fixed) |

### Catatan Penting

- Kombinasi `(config_type, payment_channel)` bersifat UNIQUE.
- Fee Singapay dengan model persentase menggunakan **reverse calculation**: buyer membayar jumlah yang sudah mencakup fee, bukan jumlah ditambah fee.

---

## 7. SettlementBatch

**Tabel:** `settlement_batches`

Merepresentasikan satu file CSV settlement dari Singapay. Mengelompokkan `settlement_items` dan melacak progress rekonsiliasi.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier batch |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `account_uuid` | VARCHAR(255) FK | Akun seller pemilik batch ini |
| `report_file_name` | VARCHAR(255) | Nama file CSV |
| `settlement_date` | DATE | Tanggal settlement dari Singapay |
| `batch_id` | VARCHAR(255) | Batch ID dari metadata CSV Singapay |
| `gross_amount` | BIGINT | Total amount sebelum fee |
| `net_amount` | BIGINT | Total PAY TO MERCHANT (setelah gateway fee) |
| `gateway_fee` | BIGINT | Total gateway fee dari semua transaksi |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `uploaded_by` | VARCHAR(255) | ID user yang mengupload |
| `uploaded_at` | TIMESTAMP | Waktu upload |
| `processed_at` | TIMESTAMP | Waktu rekonsiliasi selesai |
| `processing_status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `matched_count` | INT | Jumlah baris CSV yang berhasil dicocokkan |
| `unmatched_count` | INT | Jumlah baris CSV yang tidak cocok |
| `failure_reason` | TEXT | Detail error jika gagal |
| `metadata` | JSONB | Data tambahan dari CSV |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Lifecycle Status

```
PENDING ──► PROCESSING ──► COMPLETED
   │              │
   └──────────────┴──► FAILED
```

### Batasan

- Hanya boleh ada **satu batch per seller per tanggal settlement** (unique constraint pada `account_uuid` + `settlement_date`).

### Relasi

- `N:1` ke `ledger_accounts`
- `1:N` ke `settlement_items`
- `1:1` ke `reconciliation_discrepancies`
- Direferensikan oleh `journals` (sebagai `source_type='SETTLEMENT_BATCH'`)

---

## 8. SettlementItem

**Tabel:** `settlement_items`

Merepresentasikan satu baris dari CSV settlement Singapay. Dicocokkan ke `product_transactions` berdasarkan `invoice_number`.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier item |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `settlement_batch_uuid` | VARCHAR(255) FK | Batch induk |
| `product_transaction_uuid` | VARCHAR(255) FK | Transaksi yang dicocokkan (null jika belum cocok) |
| `seller_account_id` | VARCHAR(255) | ID akun seller (cache untuk grouping) |
| `invoice_number` | VARCHAR(100) | INVOICE NUMBER dari CSV (kunci pencocokan) |
| `sub_account` | VARCHAR(100) | SUB ACCOUNT dari CSV (ID sub-account Singapay) |
| `transaction_amount` | BIGINT | AMOUNT dari CSV |
| `pay_to_merchant` | BIGINT | PAY TO MERCHANT dari CSV (net setelah gateway fee) |
| `allocated_fee` | BIGINT | FEE dari CSV (gateway fee) |
| `is_matched` | BOOLEAN | Apakah sudah cocok dengan `product_transaction` |
| `csv_row_number` | INT | Nomor baris asli di CSV (untuk debugging) |
| `raw_csv_data` | JSONB | Data baris CSV asli |
| `expected_net_amount` | BIGINT | Jumlah yang seharusnya diterima (kalkulasi sistem) |
| `amount_discrepancy` | BIGINT | `pay_to_merchant - expected_net_amount` |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Alur Pencocokan

1. Parse CSV → buat `SettlementItem` per baris
2. Cari `product_transaction` berdasarkan `invoice_number`
3. Hitung `expected_net_amount` berdasarkan fee model transaksi
4. Bandingkan dengan `pay_to_merchant` dari CSV
5. Jika ada selisih → catat di `amount_discrepancy` dan `fee_adjustment`
6. Jika selisih signifikan → buat `ReconciliationDiscrepancy`

### Relasi

- `N:1` ke `settlement_batches`
- `N:1` ke `product_transactions` (via `invoice_number`)

---

## 9. Disbursement

**Tabel:** `disbursements`

Permintaan penarikan saldo seller ke rekening bank eksternal. Memicu debit dari bucket `AVAILABLE`.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier disbursement |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `account_uuid` | VARCHAR(255) FK | Akun seller yang melakukan penarikan |
| `amount` | BIGINT | Jumlah penarikan |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `bank_code` | VARCHAR(10) | Kode bank tujuan (misal: `"014"` untuk BCA) |
| `account_number` | VARCHAR(50) | Nomor rekening tujuan |
| `account_name` | VARCHAR(255) | Nama pemilik rekening |
| `description` | TEXT | Deskripsi transaksi (opsional) |
| `external_transaction_id` | VARCHAR(100) | ID transaksi dari Singapay |
| `failure_reason` | TEXT | Detail error jika gagal |
| `processed_at` | TIMESTAMP | Waktu Singapay memproses penarikan |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Lifecycle Status

```
PENDING ──► PROCESSING ──► COMPLETED
   │              │
   │              └──► FAILED
   ├──► COMPLETED (langsung, tanpa PROCESSING)
   ├──► FAILED
   └──► CANCELLED
```

- **`PENDING`** → **`PROCESSING`** — Singapay menerima dan sedang memproses.
- **`PROCESSING`** → **`COMPLETED`** — Singapay berhasil mentransfer.
- **`PENDING`** → **`COMPLETED`** — Singapay langsung berhasil tanpa delay.
- **`PENDING`** / **`PROCESSING`** → **`FAILED`** — Terjadi error.
- **`PENDING`** → **`CANCELLED`** — Dibatalkan sebelum diproses.

`COMPLETED`, `FAILED`, dan `CANCELLED` adalah terminal state.

### Prasyarat

- Seller harus memiliki `Verification` dengan status `APPROVED`.
- Saldo `AVAILABLE` harus mencukupi.

### Relasi

- `N:1` ke `ledger_accounts`
- Direferensikan oleh `journals` (sebagai `source_type='DISBURSEMENT'`)

---

## 10. ReconciliationDiscrepancy

**Tabel:** `reconciliation_discrepancies`

Mencatat ketidaksesuaian saldo yang terdeteksi saat rekonsiliasi settlement. Satu record per seller per batch.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier discrepancy |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `account_uuid` | VARCHAR(255) FK | Akun seller yang terpengaruh |
| `settlement_batch_uuid` | VARCHAR(255) FK | Batch yang memicu deteksi |
| `discrepancy_type` | VARCHAR(50) | Lihat tipe di bawah |
| `expected_pending` | BIGINT | Saldo PENDING yang dihitung sistem |
| `actual_pending` | BIGINT | Saldo PENDING dari Singapay GetBalance API |
| `expected_available` | BIGINT | Saldo AVAILABLE yang dihitung sistem |
| `actual_available` | BIGINT | Saldo AVAILABLE dari Singapay GetBalance API |
| `pending_diff` | BIGINT | `actual_pending - expected_pending` |
| `available_diff` | BIGINT | `actual_available - expected_available` |
| `item_discrepancy_count` | INT | Jumlah `settlement_items` dengan selisih |
| `total_item_discrepancy` | BIGINT | Total selisih dari semua item |
| `status` | VARCHAR(20) | `PENDING`, `RESOLVED`, `AUTO_RESOLVED` |
| `detected_at` | TIMESTAMP | Waktu selisih terdeteksi |
| `resolved_at` | TIMESTAMP | Waktu selisih diselesaikan |
| `resolution_notes` | TEXT | Catatan penyelesaian |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Tipe Discrepancy (`discrepancy_type`)

| Tipe | Keterangan |
|---|---|
| `PENDING_MISMATCH` | Selisih hanya di bucket PENDING |
| `AVAILABLE_MISMATCH` | Selisih hanya di bucket AVAILABLE |
| `BOTH_MISMATCH` | Selisih di kedua bucket |
| `UNEXPECTED_CREDIT` | Ada kredit yang tidak terduga |
| `UNEXPECTED_DEBIT` | Ada debit yang tidak terduga |

### Lifecycle Status

```
PENDING ──► RESOLVED (manual oleh admin)
   └──► AUTO_RESOLVED (diselesaikan otomatis oleh sistem)
```

### Batasan

- Hanya boleh ada **satu discrepancy per seller per batch** (unique constraint pada `account_uuid` + `settlement_batch_uuid`).

### Relasi

- `N:1` ke `ledger_accounts`
- `N:1` ke `settlement_batches`

---

## 11. Verification

**Tabel:** `ledger_verifications`

Data KYC (Know Your Customer) seller. Melacak upload KTP dan selfie serta status verifikasi oleh admin.

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier verifikasi |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `account_uuid` | VARCHAR(255) FK UNIQUE | Akun seller (satu per seller) |
| `identity_id` | VARCHAR(16) UNIQUE | Nomor KTP (16 digit) |
| `fullname` | VARCHAR(255) | Nama lengkap dari KTP |
| `birth_date` | DATE | Tanggal lahir dari KTP |
| `province` | VARCHAR(255) | Provinsi dari KTP |
| `city` | VARCHAR(255) | Kota dari KTP |
| `district` | VARCHAR(255) | Kecamatan dari KTP |
| `postal_code` | VARCHAR(10) | Kode pos dari KTP |
| `ktp_photo_url` | TEXT | S3 URL foto KTP |
| `selfie_photo_url` | TEXT | S3 URL foto selfie |
| `status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `approved_by` | VARCHAR(255) | ID admin yang menyetujui/menolak |
| `approved_at` | TIMESTAMP | Waktu persetujuan atau penolakan |
| `rejection_reason` | TEXT | Alasan penolakan (jika ditolak) |
| `metadata` | JSONB | Data tambahan verifikasi |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Lifecycle Status

```
PENDING ──► APPROVED
   └──► REJECTED
```

- **`PENDING`** — KTP dan selfie sudah diupload, menunggu review admin.
- **`APPROVED`** — Admin memverifikasi. Seller dapat membuat disbursement.
- **`REJECTED`** — Verifikasi gagal. Seller harus submit ulang.

### Batasan

- Satu verifikasi per seller (unique constraint pada `account_uuid`).
- Satu `identity_id` (KTP) per sistem — mencegah pendaftaran KTP duplikat.

### Path S3

- KTP: `verification/ktp/{seller_id}/ktp.{ext}`
- Selfie: `verification/kyc/{seller_id}/kyc-selfie.{ext}`

### Relasi

- `N:1` ke `ledger_accounts`

---

## Diagram Relasi

```
                          fee_configs
                         (config table,
                          no FK relations)

 ledger_accounts ◄────────────────────────────────────┐
      │                                                │
      ├──► product_transactions ◄── payment_requests  │
      │         │                                      │
      │         ▼                                      │
      │    settlement_items ◄── settlement_batches ───►│
      │                               │                │
      │                               ▼                │
      │                  reconciliation_discrepancies  │
      │                                                │
      ├──► disbursements                               │
      │                                                │
      ├──► ledger_verifications                        │
      │                                                │
      └──► ledger_entries ◄── journals                │
                                   │                   │
                                   └───────────────────┘
                             (source: product_transactions,
                              settlement_batches, disbursements)
```

---

## Alur Bisnis End-to-End

### 1. Pembayaran Produk

```
Buyer membayar
    │
    ▼
ProductTransaction (PENDING)
    + PaymentRequest (PENDING)
    │
    ▼ [money-in webhook]
ProductTransaction (COMPLETED)
    + PaymentRequest (COMPLETED)
    + Journal (PAYMENT_SUCCESS)
    + 3x LedgerEntry:
        seller  → PENDING +seller_net_amount
        platform → PENDING +platform_fee
        Singapay    → PENDING +gateway_fee
```

### 2. Rekonsiliasi Settlement

```
Admin upload CSV settlement Singapay
    │
    ▼
SettlementBatch (PENDING)
    + N x SettlementItem (per baris CSV)
    │
    ▼ [proses matching]
SettlementItem dicocokkan ke ProductTransaction (via invoice_number)
    │
    ├─[cocok, jumlah sesuai]──► ProductTransaction (SETTLED)
    │                           + Journal (SETTLEMENT)
    │                           + 2x LedgerEntry per seller:
    │                               seller PENDING -seller_net_amount
    │                               seller AVAILABLE +seller_net_amount
    │
    └─[selisih terdeteksi]──► ReconciliationDiscrepancy (PENDING)
```

### 3. Penarikan Saldo (Disbursement)

```
Seller request penarikan
    │
    ├─[cek] Verification (APPROVED)?
    ├─[cek] AVAILABLE balance mencukupi?
    │
    ▼
Disbursement (PENDING)
    + Journal (DISBURSEMENT)
    + LedgerEntry: seller AVAILABLE -amount
    │
    ▼ [Singapay proses]
Disbursement (COMPLETED)
    Dana masuk ke rekening bank seller
```

### 4. Transfer Platform Fee

```
[background job: ProcessPlatformFeeTransfer]
    │
    ▼
Cari ProductTransaction dengan:
    status = SETTLED
    platform_fee_transferred = false
    │
    ▼
Transfer via Singapay intra sub-account API
    │
    ▼
ProductTransaction.platform_fee_transferred = true
ProductTransaction.platform_fee_transferred_at = NOW()
```