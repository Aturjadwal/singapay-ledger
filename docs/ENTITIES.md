# Ledger — Entity Reference

Dokumen ini menjelaskan seluruh tabel/entitas yang terlibat dalam operasional ledger: struktur field, relasi antar entitas, lifecycle status, dan peran masing-masing dalam alur bisnis.

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
| [SettlementNotification](#7-settlementnotification) | `settlement_notifications` | Inbox webhook settlement Singapay |
| [Disbursement](#8-disbursement) | `disbursements` | Penarikan saldo seller ke rekening bank |
| [Verification](#9-verification) | `ledger_verifications` | Verifikasi KYC seller |

> **Dihapus pada 2026-09-13** oleh [migrasi 025](../database/migrations/025_drop_legacy_settlement_tables.sql):
> `settlement_batches`, `settlement_items`, `reconciliation_discrepancies`. Ketiganya adalah
> penyimpanan untuk reconciler batch DOKU, yang tidak pernah berjalan di Singapay dan
> digantikan mekanisme settlement per-transaksi — lihat
> [102](./102-settlement-reconciliation.md). Nilai `source_type = 'SETTLEMENT_BATCH'` masih ada
> di `journals` dan `ledger_entries` untuk baris historis, dan tidak boleh dihapus.

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
- `1:N` ke `ledger_entries`
- `1:1` ke `ledger_verifications`

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
| `invoice_number` | VARCHAR(50) UNIQUE | Nomor invoice; dikirim sebagai `merchant_reff_no` dan dipakai untuk mencocokkan pembayaran |
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
| `settled_at` | TIMESTAMP | Waktu Singapay mengkonfirmasi dana sudah settle |
| `settled_platform_fee` | BIGINT NULL | Platform fee setelah fee gateway sebenarnya diketahui. `NULL` = settle sebelum kolom ini ada, pakai `platform_fee`. **Bukan nol.** |
| `settled_gateway_fee` | BIGINT NULL | Fee yang benar-benar diambil Singapay. `NULL` berarti sama |
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
- **`SETTLED`** — Singapay mengkonfirmasi dana sudah settle saat ditanya per transaksi. Saldo `PENDING` berpindah ke `AVAILABLE` di sini.
- **`FAILED`** — Transaksi gagal di titik mana pun.
- **`REFUNDED`** — Dana dikembalikan ke buyer.

### Relasi

- `N:1` ke `ledger_accounts` (buyer dan seller)
- `1:1` ke `payment_requests`
- Direferensikan oleh `journals` (sebagai `source_type='PRODUCT_TRANSACTION'`)

> `status` di tabel ini adalah **satu-satunya sumber kebenaran** untuk status transaksi.
> Kedua perpindahan yang menggerakkan uang — `PENDING → COMPLETED` di money-in webhook dan
> `COMPLETED → SETTLED` di settling pass — dilakukan lewat `UpdateStatusIf`, sebuah
> compare-and-set yang memegang row lock. Pemanggil yang kalah balapan rollback tanpa menulis
> apa pun.

---

## 3. PaymentRequest

**Tabel:** `payment_requests`

Mencatat **instrumen pembayaran** apa yang diterbitkan untuk sebuah transaksi, dan kunci apa
yang dipakai untuk membaca pembayaran itu kembali dari Singapay. Satu `PaymentRequest` per
`ProductTransaction`, dibuat bersamaan dalam satu DB transaction.

> **Tidak punya status sendiri, dan itu disengaja.** Karena dibuat 1:1 dan tidak pernah
> berdiri sendiri, pertanyaan "sudah dibayar belum?" adalah pertanyaan tentang transaksinya —
> dan jawabannya diputuskan di `product_transactions.status` di bawah compare-and-set. Salinan
> kedua di sini hanya bisa setuju dengan yang itu, atau salah tentangnya. Kolom `status`,
> `failure_reason` dan `completed_at` dihapus oleh
> [migrasi 025](../database/migrations/025_drop_legacy_settlement_tables.sql).

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` | VARCHAR(255) PK | Identifier request |
| `randid` | VARCHAR(255) UNIQUE | ID acak untuk referensi publik |
| `product_transaction_uuid` | VARCHAR(255) FK | Transaksi yang ditautkan |
| `request_id` | VARCHAR(100) UNIQUE | ID **instrumen** dari Singapay (ULID VA, id QRIS/link, id e-wallet) |
| `payment_code` | TEXT | Nomor VA, atau payload EMVCo QRIS lengkap yang di-scan pembeli |
| `payment_channel` | VARCHAR(50) | `QRIS`, `VA_BCA`, `VA_BRI`, `VA_MANDIRI`, `VA_BNI`, `EWALLET_*`, `PAYMENT_LINK` |
| `payment_url` | TEXT | URL bagi buyer untuk menyelesaikan pembayaran |
| `amount` | BIGINT | Total yang dibebankan ke buyer |
| `currency` | VARCHAR(3) | `IDR` atau `USD` |
| `gateway_transaction_id` | VARCHAR(100) | Id numerik **pembayaran** dari money-in webhook |
| `gateway_transaction_ref` | VARCHAR(100) | Id bisnis **pembayaran** dari money-in webhook |
| `expires_at` | TIMESTAMP | Kadaluarsa yang diminta ke Singapay saat instrumen dibuat |
| `created_at` | TIMESTAMP | Waktu pembuatan |
| `updated_at` | TIMESTAMP | Waktu update terakhir |

### Instrumen vs. pembayaran

`request_id` menamai **instrumen**. Untuk VA dan payment link itu entitas yang berbeda dari
transaksinya: VA adalah wadah, dan pembayaran yang masuk ke dalamnya punya id bisnis sendiri;
satu payment link bisa menampung beberapa percobaan, masing-masing dengan id sendiri. Jadi
`request_id` tidak bisa dipakai membaca transaksi yang sudah settle.

`gateway_transaction_id` dan `gateway_transaction_ref` diisi dari money-in webhook — momen
pertama transaksinya benar-benar ada di Singapay untuk semua channel. Dua kolom karena keempat
endpoint detail tidak sepakat: id numerik untuk QRIS, e-wallet dan payment link; id bisnis
untuk VA. Keduanya datang di webhook yang sama, jadi menyimpan dua-duanya menghilangkan
tebakan per-channel dari jalur baca settlement. Kosong pada baris yang lebih tua dari kolom
ini, dan pembacanya memperlakukan itu sebagai "pakai lookup per-channel", bukan error.

### Tentang `expires_at`

Catatan tentang apa yang diminta ke Singapay, **bukan** lifecycle yang dijalankan ledger ini.
Tidak ada penyapu yang berjalan di atasnya. Instrumen yang lewat masa berlakunya kadaluarsa di
sisi gateway, dan transaksinya cukup tetap `PENDING`. Kalau kadaluarsa pembayaran suatu saat
benar-benar mau diimplementasikan, tempatnya di `product_transactions`, bersebelahan dengan
compare-and-set yang sudah menjaga pembukuan.

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
| `SETTLEMENT` | Singapay mengkonfirmasi dana settle (per transaksi) | 2 entri per akun: debit PENDING, kredit AVAILABLE; plus pembersihan akun gateway |
| `DISBURSEMENT` | Seller membuat permintaan penarikan | 1 entri: debit AVAILABLE seller |
| `RECONCILIATION` | Penyesuaian saldo manual. Tidak ditulis kode mana pun saat ini; nilainya dipertahankan di CHECK constraint untuk baris historis | Bervariasi |
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

- **`PENDING`** — Dana yang sudah di-capture dari buyer tetapi belum dikonfirmasi settle oleh Singapay.
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

## 7. SettlementNotification

**Tabel:** `settlement_notifications`

Inbox webhook settlement. Menyimpan setiap kiriman **apa adanya** setelah verifikasi tanda
tangan, dan melacak apakah sebuah settling pass sudah bertindak atasnya.

> Webhook-nya adalah **bel pintu, bukan sumber kebenaran.** Payload-nya membawa total dan
> rentang tanggal tanpa daftar transaksi yang dicakup, jadi tidak ada di dalamnya yang bisa
> membenarkan sebuah ledger entry. Yang dilakukannya adalah memberi tahu settling pass bahwa
> layak bertanya ke Singapay tentang invoice yang masih terbuka. Detail di
> [102](./102-settlement-reconciliation.md).

### Fields

| Field | Tipe | Keterangan |
|---|---|---|
| `uuid` / `randid` | VARCHAR(255) | Identifier |
| `settlement_id` | VARCHAR(255) | Id settlement milik Singapay |
| `settlement_reference` | VARCHAR(255) | `reference_no`, label yang dikutip saat support |
| `event` | VARCHAR(50) | `settlement.completed`, `settlement.refunded`, `settlement.refund_cancelled` |
| `settlement_method` | VARCHAR(30) | `balance`, `auto-balance`, `bank-account`, `e-wallet`. Hanya dua pertama yang memindahkan `PENDING` ke `AVAILABLE` |
| `settlement_type` | VARCHAR(20) | `ALL`, `VA`, `QRIS`, `EWALLET` |
| `start_date` / `end_date` | TIMESTAMP | Rentang yang dicakup. **Audit saja** — settling pass tidak pernah memfilter dengannya |
| `total_transactions`, `amount`, `total_fee`, `currency` | | Total sebagaimana diumumkan. Dicatat, tidak pernah dipercaya sebagai dasar entry |
| `raw_payload` | JSONB | Kiriman persis seperti datangnya |
| `status` | VARCHAR(20) | Lihat lifecycle di bawah |
| `failure_reason` | TEXT | Alasan `NEEDS_REVIEW` atau `FAILED` |
| `received_at` / `processed_at` | TIMESTAMP | |

### Lifecycle Status

```
PENDING ──► PROCESSING ──► PROCESSED
   │             │
   │             └──► FAILED (klaim dilepas, akan dicoba lagi)
   │
   └──► NEEDS_REVIEW
```

- **`PENDING`** — Tersimpan, menunggu settling pass.
- **`PROCESSING`** — Sudah diklaim satu worker. Klaim yang kalah berarti worker lain sedang
  menanganinya, dan yang benar adalah membiarkannya.
- **`PROCESSED`** — Sebuah pass sudah berjalan setelah melihatnya. **Itu saja klaimnya** —
  apakah suatu transaksi ikut settle dicatat di transaksinya, bukan di sini. Memisahkan
  keduanya disengaja: pass yang tidak tuntas tidak boleh bisa mencatat dirinya tuntas.
- **`NEEDS_REVIEW`** — Diparkir untuk manusia. Event refund (butuh kebijakan saldo negatif yang
  belum ada), atau `settlement_method` yang membayarkan ke rekening alih-alih memindahkan
  `PENDING` ke `AVAILABLE`.

### Idempotensi

`UNIQUE (settlement_id, event)`. Singapay melakukan retry, jadi kiriman ulang adalah lalu
lintas biasa: identitas ini menyerapnya lewat `ON CONFLICT DO NOTHING`, dan pemanggil tetap
dijawab sukses. Menjawab selain itu mengajari Singapay untuk terus mengulang kiriman yang
sudah diterima.

### Relasi

Tidak ada FK. Sengaja: sebuah notifikasi tidak memiliki transaksi mana pun, dan tidak ada
transaksi yang menunggu notifikasi tertentu.

---

## 8. Disbursement

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

## 9. Verification

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
      fee_configs                 settlement_notifications
   (tabel konfigurasi,          (inbox webhook, tanpa FK ke
    tanpa relasi FK)             entitas mana pun)

 ledger_accounts ◄──────────────────────────────┐
      │                                          │
      ├──► product_transactions ◄── payment_requests
      │                                          │
      ├──► disbursements                         │
      │                                          │
      ├──► ledger_verifications                  │
      │                                          │
      └──► ledger_entries ◄── journals          │
                                   │             │
                                   └─────────────┘
                             (source: product_transactions,
                              disbursements, manual_adjustment)
```

`settlement_notifications` sengaja berdiri sendiri tanpa FK: sebuah notifikasi tidak memiliki
transaksi mana pun, dan tidak ada transaksi yang menunggu notifikasi tertentu. Itu justru yang
membuat webhook bisa hilang tanpa mengakibatkan uang tertahan — lihat floor age di
[102](./102-settlement-reconciliation.md).

---

## Alur Bisnis End-to-End

### 1. Pembayaran Produk

```
Buyer membayar
    │
    ▼
ProductTransaction (PENDING)
    + PaymentRequest (instrumen diterbitkan)
    │
    ▼ [money-in webhook]
ProductTransaction (COMPLETED)      ← compare-and-set, memegang row lock
    + PaymentRequest: id transaksi gateway disimpan
    + Journal (PAYMENT_SUCCESS)
    + 3x LedgerEntry:
        seller  → PENDING +seller_net_amount
        platform → PENDING +platform_fee
        Singapay    → PENDING +gateway_fee
```

### 2. Settlement (per transaksi)

```
Webhook settlement masuk           ATAU   transaksi COMPLETED tertua > 24 jam
    │                                          │
    ▼                                          │
SettlementNotification (PENDING)               │
    │                                          │
    └──────────────► [worker tick] ◄───────────┘
                          │
                          ▼
            GetAwaitingSettlement(limit)   ← status = 'COMPLETED', tertua dulu
                          │
            ┌─────────────┴──────────────┐
            │  per invoice yang terbuka: │
            │  1 call ke Singapay        │  ← channel menentukan endpoint,
            │  has_settle == true ?      │    payment_request menentukan kunci
            └─────────────┬──────────────┘
                          │ ya
                          ▼
        satu DB transaction per ProductTransaction:
          ProductTransaction (SETTLED)        ← compare-and-set, statement pertama
            + settled_platform_fee / settled_gateway_fee
            + Journal (SETTLEMENT)            ← metadata = jejak audit fee aktual
            + LedgerEntry:
                seller   PENDING -net,  AVAILABLE +net
                platform PENDING -fee,  AVAILABLE +fee
                gateway  PENDING -gateway_fee   (dibersihkan, tanpa AVAILABLE)
                FEE_ADJUSTMENT bila fee aktual ≠ fee ekspektasi (lihat 104)
                          │
                          ▼
            SettlementNotification (PROCESSED)
```

Bila selisih fee tidak bisa diserap — platform akan menanggung lebih dari yang pernah
ditagihnya, atau seller menerima kurang dari nol — transaksi **dibiarkan `COMPLETED`** untuk
ditangani manusia. Ia tetap muncul di `GetAwaitingSettlement` dan terus menaikkan umur
tertua-yang-menunggu, yang justru kegagalan yang berisik alih-alih yang senyap.

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