# Migrasi DOKU → Singapay: Peta Endpoint & Model Operasi

> **DOKUMEN RISET, BUKAN REFERENSI KEADAAN SEKARANG.** Ditulis 2026-09-09 sebagai bahan
> migrasi; tidak diperbarui mengikuti kode. Isinya tentang **cara kerja Singapay**, yang tetap
> berlaku; yang tidak lagi berlaku adalah kesimpulan desain di dalamnya.
>
> Khususnya: pertanyaan terbuka soal settlement yang dirumuskan di sini — timezone window dan
> `settle_at` vs `settled_to_merchant_at` — **tidak dijawab, melainkan dihindari.** Jalur
> settlement yang jadi dibangun tidak pernah memakai window sama sekali. Lihat
> [102](./102-settlement-reconciliation.md).
>
> Untuk keadaan sekarang: [`../AGENTS.md`](../AGENTS.md), [`ENTITIES.md`](./ENTITIES.md).

Dokumen ini merangkum **cara kerja payment gateway Singapay** dan memetakan setiap
pemakaian DOKU di repo ini ke endpoint Singapay yang setara. Sumbernya adalah
dokumentasi publik Singapay ditambah OpenAPI spec resminya
(`https://payment-b2b.singapay.id/api/docs/merchant-api.json`, 63 path, `info.version 1.0.0`),
yang jauh lebih lengkap dan lebih akurat daripada halaman guide-nya — **kalau guide dan
spec berbeda, spec yang dipakai** (dan perbedaannya dicatat di §9).

> **Status: SUDAH DIKERJAKAN, kecuali rekonsiliasi.**
>
> Dokumen ini ditulis sebagai riset sebelum migrasi. Migrasinya sudah dilakukan: paket
> `github.com/21strive/doku` sudah dilepas, `LedgerClient` sudah memanggil Singapay untuk
> pembuatan akun, pembayaran, webhook, validasi rekening, payout, dan transfer platform fee.
> Lihat [CHANGELOG.md](../CHANGELOG.md).
>
> Yang **belum**: rekonsiliasi settlement (§7). `ProcessReconciliation` menolak jalan, jadi
> saldo `PENDING` belum pernah menjadi `AVAILABLE`. Alasannya ada di §9.1, §9.4, §9.7 dan
> §9.12 — empat pertanyaan yang harus dijawab dulu ke sandbox. Ringkasannya di
> [102-settlement-reconciliation.md](./102-settlement-reconciliation.md).
>
> Dokumen ini sengaja dipertahankan apa adanya, termasuk penyebutan gateway lama, karena
> nilainya justru pada perbandingan itu: ia mencatat **kenapa** setiap keputusan diambil dan
> apa yang masih terbuka. Referensi ke file yang sudah tidak ada (`doku_subaccount.go`,
> `DokuSettlementCSVParser`) dibiarkan sebagai catatan sejarah — penggantinya
> `singapay_account.go` dan tidak ada lagi parser CSV.

---

## 1. Ringkasan eksekutif

Singapay secara arsitektur **sangat mirip DOKU SAC** (Sub-Account) — dan itu kabar baik:
model ledger di repo ini (satu ledger account ↔ satu sub-account gateway, pending →
available, transfer antar sub-account untuk platform fee) bisa dipertahankan hampir utuh.

| Konsep di repo | DOKU hari ini | Singapay |
| --- | --- | --- |
| Sub-account penjual | `SAC-xxxx-xxxx` via `POST /sac-merchant/v1/accounts` | Account ULID via `POST /api/v1.0/accounts` |
| Halaman bayar | `POST /checkout/v1/payment` | `POST /api/v2.0/payment-link/{account_id}` |
| Konfirmasi bayar | HTTP notification DOKU | Webhook `transaction_notif_url` (`event: payment-link-transaction`) |
| Payout ke bank | `POST /sac-merchant/v1/payouts` | `POST /api/v2.0/disbursement/transfer` |
| Transfer platform fee | `POST /sac-merchant/v1/transfers` | `POST /api/v1.0/account-transfer/{account_id}/transfer` |
| Cek saldo sub-account | `GET /sac-merchant/v1/balances/{sac}` | `GET /api/v1.0/balance-inquiry/{account_id}` |
| Validasi rekening bank | `POST /snap/v1.1/emoney/bank-account-inquiry` | `POST /api/v2.0/disbursement/check-beneficiary` |
| Rekonsiliasi settlement | Upload CSV settlement | Webhook `settlement.*` + `GET /api/v1.0/statements/{account_id}` + list transaksi per produk |

Tiga hal yang **tidak** 1:1 dan perlu keputusan (detail di §9):

1. **Rekonsiliasi berubah total.** Tidak ada CSV settlement platform-wide dengan
   `INVOICE NUMBER` + `FEE` + `PAY TO MERCHANT` per baris. Penggantinya adalah kombinasi
   webhook settlement (level batch, bukan level transaksi) + polling list transaksi per
   akun per produk. Seluruh `DokuSettlementCSVParser` dan `ProcessReconciliation` harus
   ditulis ulang, bukan disesuaikan.
2. **Payment Link tidak mengekspos fee per transaksi.** VA, QRIS, dan e-wallet
   mengekspos fee per transaksi; Payment Link History tidak. Padahal
   `104-fee-mismatch-reconciliation.md` bergantung pada `ActualDokuFee` per transaksi.
3. **Pembuatan sub-account tidak pakai email dan tidak idempoten.** Body-nya cuma `name`
   + `account_type`. Tidak ada 409 duplicate seperti DOKU, jadi retry bisa membuat
   sub-account kedua yang menampung uang tanpa error apa pun.

---

## 2. Model operasi Singapay

### 2.1 Hierarki

```
Merchant (kredensial: client_id / client_secret / X-PARTNER-ID)
 └── Account (sub-account, id = ULID, punya account_number 12 digit)
      ├── saldo sendiri (held / available / pending / total)
      ├── instrumen bayar sendiri (VA, QRIS, Payment Link, e-wallet)
      ├── statement sendiri
      └── disbursement sendiri
```

Dua tipe sub-account:

| Tipe | KYB | Status awal | Nama yang tampil ke pembeli | Cocok untuk repo ini |
| --- | --- | --- | --- | --- |
| `owned` | tidak perlu | `active` | Master Account | **ya** — seller adalah unit internal platform |
| `personal_managed` | tidak perlu | `active` | Master Account | sama dengan `owned` |
| `business_managed` | wajib, via BOSS approval | `inactive` | Sub-Account | tidak — seller tidak bisa transaksi sampai KYB lolos |

**Repo ini memakai `owned`. Tidak ada approval manual apa pun.** Kirim
`account_type: "owned"`, akun langsung lahir `active` dan bisa menerima pembayaran saat
itu juga — perilakunya identik dengan `type: "STANDARD"` di DOKU sekarang, dan
`kyb_status`-nya `null`.

Ini penting karena repo membuat sub-account **di dalam booking berbayar pertama seorang
seller** ([ledger.go:126](../ledger.go#L126)) — jalur sinkron yang tidak boleh menunggu
siapa pun. `business_managed` disebut di tabel di atas justru sebagai **peringatan untuk
tidak dipilih**: namanya terdengar cocok untuk "seller pihak ketiga", padahal ia
mengembalikan akun `inactive` yang baru bisa bertransaksi setelah tim verifikasi Singapay
menyetujui KYB — yang berarti booking berbayar pertama setiap seller akan gagal.

Konsekuensi memilih `owned` yang perlu diketahui (bukan penghalang, tapi keputusan sadar):

| Hal | Dengan `owned` |
| --- | --- |
| Nama yang dilihat pembeli saat bayar | **nama Master Account (platform)**, bukan nama seller |
| Penerima invoice | Master Account |
| Konfigurasi webhook | otomatis mewarisi milik Master Account — **satu URL untuk semua seller**, tidak perlu setup per seller |
| Akses seller ke dashboard | **tidak ada, dan tidak bisa diberikan lewat API** — lihat §5.1 |

Dua yang pertama perlu dicek terhadap perilaku DOKU hari ini kalau branding seller di
halaman bayar itu penting. Yang ketiga justru menguntungkan repo ini: satu
`transaction_notif_url` untuk seluruh platform, sama seperti sekarang.

Catatan kepatuhan: `owned` secara definisi adalah "unit bisnis internal platform",
sedangkan `business_managed` ada karena regulator ingin ada KYB pada merchant pihak
ketiga yang dananya ditampung. Kalau seller di platform ini adalah entitas pihak ketiga
sungguhan, pilihan `owned` sebaiknya dikonfirmasi ke Singapay sejak awal — ini keputusan
bisnis, bukan teknis, dan lebih murah dibereskan sebelum ribuan sub-account terbentuk.

### 2.2 Siklus uang

```
Pembeli bayar
   → transaksi (VA / QRIS / Payment Link / e-wallet) berstatus paid
   → webhook ke transaction_notif_url
   → saldo masuk sebagai PENDING di akun tujuan
        (transaksi punya has_settle=false, settle_at=null)
   → batch settlement (T+0 s/d T+4 tergantung channel & tipe settlement)
        → settlement_method = balance / auto-balance → PENDING jadi AVAILABLE
        → settlement_method = bank-account          → uang keluar ke rekening bank
   → webhook settlement.completed ke settlement_notif_url
   → AVAILABLE bisa dipakai untuk disbursement / account transfer
```

Pemetaan ini penting: **`pending_balance` Singapay = `PENDING` di ledger repo,
`available_balance` Singapay = `AVAILABLE` di ledger repo.** Artinya asumsi inti
`102-settlement-reconciliation.md` tetap berlaku — yang berubah cuma *sinyal* pemicunya.

`held_balance` adalah konsep baru yang tidak ada di DOKU (dana ditahan). Perlu
dikonfirmasi kapan terisi; kalau ikut terhitung, `GetAllBalances` bisa mismatch.

### 2.3 Environment

| Env | Base URL |
| --- | --- |
| Sandbox | `https://sandbox-payment-b2b.singapay.id` |
| Production | `https://payment-b2b.singapay.id` |

Semua path punya prefix `/api`. **IP server wajib di-whitelist** di dashboard merchant
sebelum request produksi diterima (`SP017` kalau tidak) — ini beda operasional dari DOKU
dan harus masuk checklist deploy.

---

## 3. Autentikasi & signature

Ada **tiga** skema kriptografi yang berbeda. Jangan dicampur.

### 3.1 Access token (`POST /api/v1.1/access-token/b2b`)

```
payload     = "{client_id}_{client_secret}_{YYYYMMDD}"   // tanggal Asia/Jakarta
X-Signature = HMAC-SHA512(payload, client_secret)        // hex lowercase
```

Header: `X-PARTNER-ID`, `X-CLIENT-ID`, `X-Signature`. Body: `{"grant_type":"client_credentials"}`.

Respons: `data.access_token`, `data.token_type` (`Bearer`), `data.expires_in` (**string**,
contoh `"216000"` = 60 jam).

> Tersedia juga `POST /api/v1.0/access-token/b2b` dengan Basic auth
> (`Authorization: Basic base64(client_id:client_secret)`) untuk integrasi lama. v1.1
> lebih aman karena client_secret tidak pernah dikirim.

Token berumur 60 jam artinya cache token jelas berguna — beda dengan
[ledger.go:363](../ledger.go#L363) yang mengambil token DOKU baru setiap kali validasi
rekening.

### 3.2 Request signature (endpoint money-out)

```
1. sort key body secara rekursif & alfabetis
2. hashed_body     = SHA256(minified_json)                     // hex
3. string_to_sign  = "{METHOD}:{ENDPOINT}:{ACCESS_TOKEN}:{hashed_body}:{TIMESTAMP}"
4. X-Signature     = HMAC-SHA512(string_to_sign, client_secret) // hex lowercase
```

- `ENDPOINT` = path **termasuk query string**, tanpa domain.
- `ACCESS_TOKEN` = token tanpa prefix `Bearer `.
- Header wajib: `X-PARTNER-ID`, `Authorization: Bearer ...`, `X-Timestamp`, `X-Signature`.

Wajib di: `POST /api/v2.0/disbursement/transfer`, `POST /api/v2.0/ewallet/trigger-topup`,
`POST /api/v1.0/account-transfer/{account_id}/transfer`, QRIS issuer, Direct Debit charge.

> ⚠️ **Ambiguitas dokumentasi.** Halaman *Building the X-Signature Header* bilang
> `X-Timestamp` adalah "Unix timestamp in **seconds**". Deskripsi parameter di OpenAPI
> spec untuk `POST /api/v2.0/disbursement/transfer` bilang "**ISO-8601** timestamp".
> Keduanya tidak mungkin benar bersamaan. Harus dites di sandbox sebelum menulis client —
> ini persis kelas bug yang bikin `SP016` sulit didiagnosis.

### 3.3 Webhook signature (inbound)

Sama persis dengan §3.2, tapi kita yang **memverifikasi**:

- `METHOD` selalu `POST`, `ENDPOINT` = path webhook kita (harus cocok persis dengan yang
  didaftarkan di dashboard, termasuk query string).
- `ACCESS_TOKEN` diambil dari header `Authorization` request masuk, **strip `Bearer `**.
  Untuk webhook sistem (VA, settlement) token ini adalah **string acak**, bukan token kita
  — pakai apa adanya sebagai komponen signature.
- Bandingkan dengan `hmac.Equal` (constant time).
- Validasi `X-Timestamp` dalam 5 menit untuk anti-replay.
- Balas 2xx cepat, proses di background.

Ini secara struktural mirip `HandleNotification` DOKU ([payment.go:456](../payment.go#L456))
yang menerima `Request-Id` / `Request-Timestamp` / `Signature` / `Request-Target` /
`Json-Body` — bedanya normalisasi body (sort key rekursif + SHA-256) dan HMAC-SHA512.

---

## 4. Katalog endpoint lengkap

Semua path relatif terhadap base URL, semua diawali `/api`. Kolom **Sign** = butuh
`X-Signature` + `X-Timestamp` per request.

### Catatan versi: `v1.0` dan `v2.0` bukan dua generasi API

Ini terlihat aneh di awal — kita akan memanggil `check-fee` di `v1.0` lalu `transfer` di
`v2.0` dalam satu alur. Itu bukan salah baca dan bukan tanda API-nya belum matang:
**Singapay memberi versi per modul, bukan per API secara global.** Tidak ada "API v2"
yang menggantikan "API v1".

| Modul | Versi yang ada |
| --- | --- |
| Accounts, Balance, Statement, Account Transfer, Virtual Account, VA Transaction, QRIS money-in, Cardless Withdrawal | **hanya `v1.0`** |
| Card, Subscription, Direct Debit, E-Wallet money-out, QRIS money-out | **hanya `v2.0`** |
| Disbursement, Payment Link, E-Wallet money-in | **keduanya** |
| Access token | `v1.0` (Basic auth) dan `v1.1` (X-Signature) |

Jadi mayoritas endpoint yang repo ini butuhkan — sub-account, saldo, statement, transfer
antar akun — **tidak punya versi lain untuk dipilih**. Mencampur bukan pilihan desain
kita, itu bentuk API-nya.

**Yang penting: v1 dan v2 menunjuk data yang sama, bukan sistem terpisah.** Buktinya ada
di spec sendiri:

- Disbursement **tidak punya list/show di v2**. Satu-satunya cara membaca daftar payout
  adalah `GET /api/v1.0/disbursement/{account_id}` — yang berarti transfer yang dibuat
  lewat v2 pasti terbaca di sana, kalau tidak transaksi v2 jadi tak terlihat sama sekali.
- `POST /api/v1.0/disbursement/{account_id}/inquiry-status` **sudah memakai envelope v2**
  (`response_code` / `SP000`) dan dokumentasinya menyebut service layer yang sama
  (`DisbursementService::formatDisbursementData`).
- Payment Link: spec menyatakan `data.id` dipakai sebagai `{payment_link_id}` (v1)
  **atau** `{link_id}` (v2), dan ada field `source` bernilai `api v1` / `api v2` /
  `dashboard`. Satu tabel, dua kontrak tulis.

Yang **tidak** aman adalah mencampur *kontrak*-nya tanpa sadar. Untuk disbursement,
bedanya nyata:

| | `v1.0` transfer | `v2.0` transfer |
| --- | --- | --- |
| `account_id` | di path | di body |
| Identitas bank | `bank_swift_code` — **SWIFT saja** | `bank_code` — 3 digit **atau** SWIFT |
| Panjang no. rekening | 6–20 | 6–30 |
| Referensi duplikat | HTTP **409** | HTTP 400 + **`SP004`** |
| Envelope | `{status, success, data}` | `{response_code, response_message, data}` |
| Status transaksi | `data.status` string (`pending`/`success`/`failed`) | `data.transaction_status.{code,desc}` (`00`/`03`/`06`) |
| Fee | `data.fee.name` ada | tanpa `name` |
| `notes` | maks 50 karakter | tidak disebutkan |

Aturan yang dipakai dokumen ini:

1. **Untuk menulis (money movement), selalu pilih versi tertinggi yang tersedia.** Kode
   status `00`–`07` dan `SP000`–`SP020` jauh lebih presisi daripada string
   `pending`/`success`/`failed` di v1 — dan §5.4 menunjukkan perbedaan itu menentukan
   apakah saldo seller dilepas atau tertahan selamanya.
2. **Untuk membaca, pakai apa pun yang ada** — sering kali cuma v1 yang punya.
3. **Standarkan pada SWIFT code** untuk identitas bank. `v2.0/transfer` menerima 3 digit
   maupun SWIFT, tapi `v1.0/check-fee` **hanya menerima SWIFT** — dan check-fee tidak
   punya versi v2. Kalau menyimpan sandi bank 3 digit, alur "cek fee lalu transfer" akan
   patah di langkah pertama. Simpan SWIFT, satu format untuk semua.

### 4.1 Security

| Method | Path | Keterangan |
| --- | --- | --- |
| POST | `/api/v1.1/access-token/b2b` | Token via X-Signature (disarankan) |
| POST | `/api/v1.0/access-token/b2b` | Token via Basic auth (legacy) |

### 4.2 Accounts (sub-account)

| Method | Path | Keterangan |
| --- | --- | --- |
| POST | `/api/v1.0/accounts` | Buat sub-account |
| GET | `/api/v1.0/accounts` | List sub-account |
| GET | `/api/v1.0/accounts/{id}` | Detail sub-account (`{id}` = ULID) |
| PATCH | `/api/v1.0/accounts/update/{id}` | Ubah `name` / `status` / `invite_members` |

Tidak ada endpoint DELETE — penonaktifan dilakukan dengan `status: "inactive"`.

### 4.3 Balance & statement

| Method | Path | Keterangan |
| --- | --- | --- |
| GET | `/api/v1.0/balance-inquiry` | Saldo agregat merchant |
| GET | `/api/v1.0/balance-inquiry/{account_id}` | Saldo satu sub-account |
| GET | `/api/v1.0/statements/{account_id}` | Statement (filter `start_date`, `end_date`) |
| GET | `/api/v1.0/statements/{account_id}/{statement_id}` | Detail satu baris statement |

### 4.4 Account transfer (antar sub-account)

| Method | Path | Sign | Keterangan |
| --- | --- | :---: | --- |
| POST | `/api/v1.0/account-transfer/{account_id}/transfer` | ✔ | Transfer dari `{account_id}` ke `beneficiary_account_number` |
| GET | `/api/v1.0/account-transfer/{account_id}` | | List transfer (remitter atau beneficiary) |
| GET | `/api/v1.0/account-transfer/{account_id}/{transaction_id}` | | Detail transfer |

### 4.5 Accept money

| Produk | Method | Path |
| --- | --- | --- |
| Payment Link | POST | `/api/v2.0/payment-link/{account_id}` (create v2) |
| | POST | `/api/v1.0/payment-link-manage/{account_id}` (create v1, legacy) |
| | GET | `/api/v2.0/payment-link/{link_id}` · `/api/v1.0/payment-link-manage/{account_id}/{payment_link_id}` |
| | PUT | `/api/v2.0/payment-link/update/{link_id}` · `/api/v1.0/payment-link-manage/{account_id}/{payment_link_id}` |
| | DELETE | `/api/v1.0/payment-link-manage/{account_id}/{payment_link_id}` |
| | GET | `/api/v1.0/payment-link-manage/{account_id}` (list) |
| | GET | `/api/v1.0/payment-link-manage/payment-methods` (katalog channel) |
| Virtual Account | POST | `/api/v1.0/virtual-accounts/{account_id}` |
| | GET/PUT/DELETE | `/api/v1.0/virtual-accounts/{account_id}/{virtual_account_id}` |
| | GET | `/api/v1.0/virtual-accounts/{account_id}` (list) |
| QRIS | POST | `/api/v1.0/qris-dynamic/{account_id}/generate-qr` |
| | GET | `/api/v1.0/qris-dynamic/{account_id}` · `/{account_id}/show/{id}` |
| E-Wallet | POST | `/api/v2.0/ewallet-native/create-order` (account_id di body) |
| | POST | `/api/v1.0/ewallet-native/{account_id}/create-checkout` (legacy) |
| | GET | `/api/v1.0/ewallet-native/{account_id}/inquiry-status/{id}` |
| Kartu | POST | `/api/v2.0/card/{account_id}/payment` |
| | PATCH | `/api/v2.0/card/{account_id}/cancel/{id}` |
| | GET | `/api/v2.0/card/{account_id}/inquiry-status/{id}` |
| Subscription | POST | `/api/v2.0/recurring/plans` |
| | GET/PATCH | `/api/v2.0/recurring/plans/{id}` |
| | POST | `/api/v2.0/recurring/plans/cancel/{id}` |
| Direct Debit | POST | `/api/v2.0/direct-debit/binding` · `/charge` · `/verify-otp` · `/binding/{id}/unbind` |
| | GET | `/api/v2.0/direct-debit/binding/{binding_id}` · `/transaction/{transaction_id}` |

> Direct Debit dan Cardless Withdrawal ditandai **SOON** di dokumentasi. Jangan
> dimasukkan rencana rilis.

### 4.6 Send money

| Method | Path | Sign | Keterangan |
| --- | --- | :---: | --- |
| POST | `/api/v2.0/disbursement/transfer` | ✔ | Payout ke bank (account_id di body) |
| POST | `/api/v2.0/disbursement/check-beneficiary` | | Name inquiry rekening |
| POST | `/api/v2.0/disbursement/{account_id}/inquiry-status` | | Cek status via `reference_number` |
| POST | `/api/v1.0/disbursement/{account_id}/check-fee` | | Kuotasi fee sebelum transfer |
| POST | `/api/v1.0/disbursement/{account_id}/transfer` | | Payout v1 (legacy) |
| GET | `/api/v1.0/disbursement/{account_id}` | | List disbursement (12 bulan terakhir, 25/hal) |
| GET | `/api/v1.0/disbursement/{account_id}/{transaction_id}` | | Detail disbursement |
| POST | `/api/v2.0/ewallet/trigger-topup` | ✔ | Payout ke e-wallet |
| POST | `/api/v2.0/ewallet/account-inquiry` | | Name inquiry e-wallet |
| POST | `/api/v2.0/qris/issuer/mpm/payment-credit` | ✔ | Bayar QRIS merchant lain |

Catatan: `check-fee` hanya ada di **v1** (path `{account_id}`), sedangkan `transfer` yang
dianjurkan adalah **v2** (`account_id` di body). Jadi alur "cek fee lalu transfer" memakai
dua versi API sekaligus — lihat *Catatan versi* di awal §4 untuk alasannya dan untuk
konsekuensi terpentingnya: **simpan kode bank dalam format SWIFT**, karena `check-fee`
tidak menerima sandi bank 3 digit sementara `v2/transfer` menerima keduanya.

### 4.7 Transaction records (dasar rekonsiliasi)

| Method | Path | Fee per transaksi? |
| --- | --- | --- |
| GET | `/api/v1.0/va-transactions/{account_id}` | ✔ `fees.amount` |
| GET | `/api/v1.0/va-transactions/{account_id}/{transaction_id}` | ✔ |
| GET | `/api/v1.0/va-transactions/{account_id}/detail-by-va-number/{va_number}` | ✔ |
| GET | `/api/v1.0/qris-dynamic/{account_id}` | ✔ `mdr_cost`, `vendor_fee`, `our_margin`, `settled_to_merchant_amount` |
| GET | `/api/v1.0/ewallet-native-transactions/{account_id}` | ✔ `merchant_fee`, `net_amount` |
| GET | `/api/v1.0/payment-link-histories/{account_id}` | ✘ **tidak ada** |
| GET | `/api/v1.0/payment-link-histories/{account_id}/{history_id}` | ✘ **tidak ada** |

Semuanya punya `has_settle` + `settle_at` dan filter `settle_at_from` / `settle_at_to`.
Ini pengganti paling dekat untuk kolom `PAY OUT DATE` di CSV DOKU.

### 4.8 Webhook

Dikonfigurasi di dashboard. URL bisa dipakai bersama beberapa event — **routing dengan
field `event` di body**.

| URL dashboard | `event` | Isi |
| --- | --- | --- |
| `transaction_notif_url` | `va-transaction` | pembayaran VA masuk |
| | `qris-acquirer-transaction` | pembayaran QRIS masuk |
| | `payment-link-transaction` | pembayaran Payment Link masuk |
| | `ewallet-native-transaction` | pembayaran e-wallet masuk |
| `disbursement_notif_url` | `disbursement` | hasil payout bank |
| | `ewallet-topup` | hasil payout e-wallet |
| | `qris-issuer` | hasil QRIS money-out |
| `settlement_notif_url` | `settlement.completed` | batch settlement selesai |
| | `settlement.refunded` | satu transaksi ditarik balik dari saldo |
| | `settlement.refund_cancelled` | refund dibatalkan, saldo dikembalikan |
| `subscription_cycle_notif_url` | — | tiap siklus tagihan berulang |
| `kyb_notif_url` | — | hasil KYB sub-account managed |
| `payment_link_inquiry_notif_url` | — | link dibuka/dilihat (opsional) |
| `product_expiration_notif_url` | — | link/VA/QRIS kedaluwarsa (batch) |
| `transaction_expiration_notif_url` | — | transaksi money-in kedaluwarsa (batch) |

---

## 5. Peta 1:1 dari pemakaian DOKU di repo ini

### 5.1 Buat sub-account penjual — [ledger.go:126](../ledger.go#L126)

| | DOKU | Singapay |
| --- | --- | --- |
| Endpoint | `POST /sac-merchant/v1/accounts` | `POST /api/v1.0/accounts` |
| Body | `{account:{email,type:"STANDARD",name}}` | `{name, account_type:"owned"}` — **tanpa email** |
| ID hasil | `SAC-xxxx-xxxx` | ULID, mis. `01K946KF851RK7FX075GJHBVKF` |
| Duplikat | 409 kalau email sudah dipakai | **tidak ada proteksi** |

```json
POST /api/v1.0/accounts
{ "name": "Budi Santoso", "account_type": "owned" }

→ 200
{ "status": 200, "success": true,
  "data": { "id": "01K946KF851RK7FX075GJHBVKF",
            "account_number": "000000000123",
            "name": "Budi Santoso", "status": "active",
            "account_type": "owned", "kyb_status": null } }
```

Dampak ke kode:

- **`sanitizeSubAccountName` dan `validateSubAccountEmail` di
  [doku_subaccount.go](../doku_subaccount.go) menjadi tidak relevan.** Batasan DOKU
  (email ≤ 40 karakter, nama huruf saja ≤ 100) tidak ada padanannya di skema Singapay.
  Jangan dibawa migrasi tanpa dikonfirmasi ulang — sanitasi yang tidak dibutuhkan justru
  memotong nama seller tanpa alasan. `validateSubAccountEmail` akhirnya dihapus, karena
  emailnya sendiri tidak lagi dikirim (poin berikutnya).
- **`invite_members` tidak bisa dipakai untuk seller, dan email seller tidak boleh
  dikirim sama sekali.** Ini baru ketahuan saat mencoba di staging: implementasi awal
  mengirim `invite_members: [email seller]`, dan Singapay menolaknya dengan

  ```
  http 422: One or more emails do not belong to a member of this merchant.
  ```

  `invite_members` bukan undangan ke orang luar — ia hanya memberi akses dashboard ke
  alamat yang **sudah menjadi member merchant kita**. Email seller tidak akan pernah
  memenuhi syarat itu, dan tidak ada endpoint untuk menambah member merchant (itu
  dilakukan di dashboard). Efeknya fatal karena `CreateAccount` dipanggil di dalam
  booking berbayar **pertama** setiap seller: selama field ini terkirim, booking pertama
  setiap seller baru gagal dengan 500. Body-nya sekarang benar-benar hanya
  `{name, account_type}` — sama seperti contoh di atas dan sama seperti yang sudah
  dipakai `cmd/singapay-smoke`.
- **`Account.DokuSubAccountID` harus menyimpan dua nilai, bukan satu**: `id` (ULID, untuk
  path parameter dan `account_id` di body) **dan** `account_number` (12 digit, satu-satunya
  cara menunjuk akun tujuan di account transfer). `account_number` bertipe nullable di
  respons — kalau kosong, akun itu tidak bisa jadi penerima transfer.
- **Tidak ada idempotency.** DOKU menolak email duplikat dengan 409, dan
  [ledger.go:126](../ledger.go#L126) memang mengandalkan itu. Di Singapay, `CreateAccount`
  yang di-retry setelah timeout akan membuat sub-account **kedua** yang menampung uang,
  tanpa error. Penjaga satu-satunya adalah pengecekan `GetByOwner` di baris 106 — yang
  tidak menolong kalau proses mati setelah panggilan API sebelum commit DB. Perlu
  ditanyakan ke Singapay apakah ada idempotency key.

### 5.2 Buat pembayaran — [payment.go:139](../payment.go#L139) & [payment.go:322](../payment.go#L322)

Padanan terdekat `POST /checkout/v1/payment` DOKU adalah **Payment Link v2**: sama-sama
menghasilkan URL checkout yang dihosting gateway, dengan pembeli memilih channel.

```json
POST /api/v2.0/payment-link/{account_id}
{
  "reff_no": "INV-20260908120000-AB12CD",     // = invoiceNumber kita
  "payment_link_type": "total",
  "total_amount": 11247,                       // = feeBreakdown.TotalCharged
  "max_usage": 1,
  "expired_at": "2026-09-08T13:00:00+07:00",   // ISO-8601, BUKAN menit
  "customer_name": "Andi",
  "customer_email": "andi@example.com",
  "whitelisted_payment_method": ["VA_BRI"],    // kosongkan agar semua channel aktif
  "success_redirect_url": "https://app.example.com/paid",
  "expired_redirect_url": "https://app.example.com/expired",
  "optional_metadata": { "product_transaction_id": "..." }
}

→ data.id (numerik), data.reff_no, data.payment_url, data.status, data.expired_at
```

Pemetaan field:

| `DokuCreatePaymentRequest` | Payment Link v2 |
| --- | --- |
| `Amount` | `total_amount` |
| `InvoiceNumber` | `reff_no` |
| `SacID` | `{account_id}` di path |
| `PaymentDueDate` (menit) | `expired_at` (timestamp absolut) — **hitung sendiri dari `ExpiresIn`** |
| `CustomerName` / `CustomerEmail` | `customer_name` / `customer_email` (hanya untuk `max_usage: 1`) |
| `PaymentMethod` | `whitelisted_payment_method: [kode]` |
| `dokuResp.Response.Payment.URL` | `data.payment_url` |
| `dokuResp.Response.Payment.TokenID` / `Order.SessionID` | `data.id` (numerik) — simpan sebagai `PaymentRequest.gateway_request_id` |

Hal yang perlu diperhatikan:

- **`GeneratePayment` memakai `PaymentChannel` yang sudah ditentukan**
  ([payment.go:139](../payment.go#L139)), sementara `GenerateSubscriptionPayment`
  mengirim channel kosong ([payment.go:322](../payment.go#L322)). Keduanya terlayani:
  `whitelisted_payment_method` diisi satu kode untuk kasus pertama, dikosongkan untuk
  kasus kedua.
- Kode channel **harus diambil dari `GET /api/v1.0/payment-link-manage/payment-methods`**
  (`VA_BRI`, `QRIS`, dst.), bukan dari konstanta DOKU. Tabel `fee_configs` harus
  di-remap seluruhnya; `domain.NewFeeCalculator` sendiri tidak berubah.
- Kalau channel sudah pasti, alternatif yang lebih presisi adalah memakai endpoint produk
  langsung (`POST /api/v1.0/virtual-accounts/{account_id}` atau
  `POST /api/v1.0/qris-dynamic/{account_id}/generate-qr`). Keuntungannya besar untuk
  rekonsiliasi: **kedua produk itu mengekspos fee per transaksi, Payment Link tidak**
  (lihat §9.2). VA juga langsung memberi nomor VA, mengisi `PaymentRequest.PaymentCode`
  yang saat ini selalu kosong di kedua fungsi generate.

### 5.3 Notifikasi pembayaran sukses — [payment.go:456](../payment.go#L456)

Webhook `POST` ke `transaction_notif_url`. Route dengan `event`.

```json
{
  "status": 200, "success": true,
  "event": "payment-link-transaction",
  "timestamp": "10 Nov 2025 09:46:38",
  "data": {
    "transaction": {
      "reff_no": "18917720251110094037705",     // ref percobaan bayar, BUKAN reff_no link
      "type": "pl", "status": "paid",
      "amount": { "value": "10000.00", "currency": "IDR" },
      "post_timestamp": "10 Nov 2025 09:46:38",
      "processed_timestamp": "10 Nov 2025 09:46:38"
    },
    "customer": { "name": "...", "email": "...", "phone": "..." },
    "payment": { "method": "payment_link",
      "additional_info": { "payment_link": { "id": 189, "reff_no": "PL2025...", "account_id": 35, ... } } }
  }
}
```

Untuk VA payloadnya beda bentuk dan **lebih berguna**:

```json
{ "event": "va-transaction",
  "data": { "transaction": { "reff_no": "INV-2026-001",          // = merchant_reff_no kita
                             "transaction_id": "3211120250926133543246",
                             "status": "paid",
                             "amount": { "value": 100000, "currency": "IDR" } },
            "payment": { "method": "va",
                         "additional_info": { "va_number": "...", "bank": {...},
                                              "fees": { "amount": 1500, "currency": "IDR" } } } } }
```

Dampak ke `HandlePaymentSuccess`:

- Kunci pencocokan invoice berubah. DOKU memberi `Order.InvoiceNumber` langsung. Di
  Singapay, untuk VA/QRIS/e-wallet kuncinya `data.transaction.reff_no` (= `merchant_reff_no`
  yang kita kirim), tapi untuk Payment Link `data.transaction.reff_no` adalah **ref
  percobaan bayar**, dan `reff_no` kita ada di
  `data.payment.additional_info.payment_link.reff_no`. Dua jalur ekstraksi berbeda.
- Cek status berubah dari `"SUCCESS"` menjadi `"paid"`.
- `dokuResp.Transaction.Status` diganti verifikasi signature lokal (§3.3) — Singapay tidak
  punya endpoint "validate notification" seperti DOKU, jadi validasi sepenuhnya di sisi kita.
- **Amount bertipe tidak konsisten**: `"10000.00"` (string) di Payment Link, `100000`
  (number) di VA. Parser harus menerima keduanya, persis seperti `FlexibleInt` yang sudah
  ada di client DOKU.
- Idempotensi lewat `productTx.IsPending()` tetap berlaku dan tetap perlu — webhook
  Singapay punya mekanisme retry.

### 5.4 Payout ke rekening bank — [ledger.go:726](../ledger.go#L726)

```json
POST /api/v2.0/disbursement/transfer          // + X-Signature + X-Timestamp
{
  "account_id": "01K946KF851RK7FX075GJHBVKF",  // ULID sub-account seller
  "reference_number": "<disbursement.UUID>",   // idempotency key
  "bank_code": "002",                          // 3 digit atau SWIFT
  "bank_account_number": "1234567890",
  "amount": 50000,                             // NET yang diterima penerima
  "notes": "Withdrawal"
}

→ { "response_code": "SP000",
    "data": { "transaction_id": "...", "reference_number": "...",
              "transaction_status": { "code": "00", "desc": "Success" },
              "gross_amount": {...}, "fee": {...}, "net_amount": {...},
              "balance_after": {...},
              "failed_code": null, "failed_reason": null } }
```

| DOKU | Singapay |
| --- | --- |
| `Request-Id` header sebagai idempotency | `reference_number` di body |
| `account.id` = SAC | `account_id` = ULID |
| `payout.invoice_number` = `disbursement.UUID` | `reference_number` = `disbursement.UUID` |
| `beneficiary.bank_account_name` **wajib dikirim** | tidak ada — nama diambil dari inquiry bank |
| status `SUCCESS` / `FAILED` / `REJECTED` / lainnya | `transaction_status.code` `00`/`06`/`01`,`02`,`03`/`04`,`05`,`07` |

Pemetaan status untuk switch di [ledger.go:739](../ledger.go#L739):

| Kode | Status | Terminal | Aksi di ledger |
| --- | --- | :---: | --- |
| `00` | Success | ya | `MarkCompleted` |
| `06` | Failed | ya | `MarkFailed` + lepas reservasi |
| `05` | Canceled | ya | `MarkFailed` + lepas reservasi |
| `04` | Refunded | ya | `MarkFailed` + lepas reservasi |
| `07` | Not Found | ya | `MarkFailed` + lepas reservasi |
| `01`/`02`/`03` | Initiated/Paying/Pending | tidak | `MarkProcessing` — **jangan** lepas reservasi |

Ini memperbaiki satu hal: kode sekarang memperlakukan semua status non-`SUCCESS`/`FAILED`/
`REJECTED` sebagai `MarkProcessing`. Singapay memberi `04`, `05`, dan `07` yang **terminal
dan gagal** — kalau diperlakukan sebagai processing, uang seller tertahan selamanya.

Tambahan penting yang tidak dimiliki DOKU:

- **`amount` milik Singapay adalah net, fee ditambahkan di atasnya.** Sub-account didebit
  `net + fee`. Pertanyaan yang tersisa waktu itu — apakah `WithdrawRequest.Amount` berarti
  "yang diterima seller" atau "yang dipotong dari saldo" — **sudah dijawab**: `Amount`
  adalah nominal yang diminta seller dan seluruh potongan saldonya, dan fee dipotong dari
  dalamnya. Seller minta tarik Rp 15.000 dengan fee Rp 3.000 → saldo berkurang Rp 15.000,
  yang dikirim ke Singapay Rp 12.000, yang diterima Rp 12.000. Jadi `amount` memang
  "dihitung mundur": `POST /api/v1.0/disbursement/{account_id}/check-fee` dipanggil sebelum
  reservasi, dan yang direservasi adalah **nominal permintaan**, bukan permintaan + fee.
  Lihat `docs/103-withdrawal-disbursement.md` dan migrasi 028.
- **`SP004` = duplicate reference number.** Ini adalah jawaban idempotensi untuk
  `RetryDisbursement` ([ledger.go:574](../ledger.go#L574)): kalau `SP004` muncul, panggil
  `POST /api/v2.0/disbursement/{account_id}/inquiry-status` dengan `reference_number` yang
  sama dan bukukan hasilnya. Ini lebih tegas daripada replay `Request-Id` DOKU.
- Ada webhook `disbursement` di `disbursement_notif_url` dengan payload yang sama persis
  dengan respons transfer. Untuk status non-terminal, ini menggantikan polling — repo
  belum punya handler webhook disbursement sama sekali.

### 5.5 Transfer platform fee antar sub-account — [ledger.go:1590](../ledger.go#L1590) & [ledger.go:2146](../ledger.go#L2146)

```json
POST /api/v1.0/account-transfer/{seller_account_ulid}/transfer    // + X-Signature
{
  "amount": 1000,
  "beneficiary_account_number": "000000000999",   // account_number platform, 12 digit
  "merchant_ref_no": "PF-INV-20260908120000-AB12CD"
}

→ { "data": { "transaction_id": "...", "merchant_ref_no": "...", "status": "success",
              "remitter": { "account_id": "...", "balance_after": "..." },
              "beneficiary": { "account_id": "...", "balance_after": "..." } } }
```

| DOKU | Singapay |
| --- | --- |
| `transfer.origin` = SAC asal | `{account_id}` di path (ULID) |
| `transfer.destination` = SAC tujuan | `beneficiary_account_number` (**account_number, bukan ULID**) |
| `transfer.invoice_number` | `merchant_ref_no` |
| `Request-Id` header | `merchant_ref_no` juga berperan sebagai idempotency key |

Ini lebih bersih daripada DOKU. Idempotensi Singapay eksplisit: **`merchant_ref_no` yang
diulang mengembalikan transfer aslinya dengan HTTP 200, bukan error**, dan unik per
merchant lintas semua akun. Artinya `platformFeeInvoiceNumber()` di
[doku_subaccount.go:36](../doku_subaccount.go#L36) tetap relevan dan alasannya tetap
berlaku — prefix `PF-` mencegah tabrakan namespace dengan invoice pembayaran, dan
determinismenya sekarang justru **menjadi mekanisme idempotensi utama**, bukan sekadar
pelengkap `Request-Id`. Kolom `transfer_request_id` yang disimpan
`SaveTransferRequestID` bisa dihapus atau diisi `merchant_ref_no`.

Yang harus disiapkan: platform account perlu `account_number`-nya tersimpan, bukan hanya
ULID. Catatan runbook T6c/T9 di [ledger.go:158](../ledger.go#L158) tentang provisioning
manual platform account perlu diperbarui untuk mencatat kedua nilai itu.

### 5.6 Cek saldo sub-account — [ledger.go:1696](../ledger.go#L1696)

```
GET /api/v1.0/balance-inquiry/{account_id}

→ data.held_balance / available_balance / pending_balance / balance
    masing-masing { "value": "1234.56", "currency": "IDR" }
```

Perbedaan yang menggigit: **nilainya string desimal 2 angka di belakang koma**
(`"1234.56"`), sedangkan ledger repo bekerja dengan `int64` rupiah bulat.
`fmt.Sscanf(..., "%d", &dokuPending)` di [ledger.go:1702](../ledger.go#L1702) akan
membaca `"1234.56"` sebagai `1234` dan diam saja — pembanding saldo jadi salah tanpa
error. Harus diganti parser desimal yang eksplisit, dan diputuskan apakah repo pindah ke
satuan sen.

`held_balance` tidak punya padanan di ledger. Perlu dikonfirmasi apakah ia bagian dari
`balance` (total) atau terpisah, sebelum dipakai membandingkan.

### 5.7 Validasi rekening bank — [ledger.go:378](../ledger.go#L378)

```json
POST /api/v2.0/disbursement/check-beneficiary
{ "bank_code": "002", "bank_account_number": "1234567890" }

→ { "response_code": "SP000",
    "data": { "bank_code": "002", "bank_account_number": "...",
              "status": "valid",                  // atau "invalid"
              "bank_account_name": "BUDI SANTOSO",
              "bank_name": "BRI",
              "message": null } }
```

Lebih sederhana daripada SNAP DOKU: tidak perlu `partnerReferenceNo`, tidak perlu `amount`,
tidak perlu token terpisah. `ValidateBankAccountResponse.IsValid` dipetakan dari
`data.status == "valid"` — bukan dari ada/tidaknya error seperti sekarang, karena Singapay
mengembalikan HTTP 200 + `status: "invalid"` untuk rekening yang tidak ditemukan.

`bank_code` menerima **3 digit sandi bank** (`002`, `014`) **atau SWIFT** (`BRINIDJA`).

### 5.8 Daftar bank — [analytics/etl_static_dim.go:258](../analytics/etl_static_dim.go#L258)

**Tidak ada endpoint daftar bank di API merchant Singapay.** `GetSupportedBanks()` yang
sekarang membaca konstanta dari library DOKU tidak punya pengganti langsung. Pilihannya:
minta daftar resmi ke Singapay dan simpan sebagai konstanta sendiri, atau turunkan dari
`GET /api/v1.0/payment-link-manage/payment-methods` (itu channel money-in, bukan bank
tujuan payout — jadi bukan pengganti yang setara). Ini item yang harus ditanyakan.

---

## 6. Rekonsiliasi & settlement

Bagian yang paling berubah. Alur sekarang (`102-settlement-reconciliation.md`) adalah:
unggah CSV DOKU → parse baris → cocokkan `INVOICE NUMBER` → tulis entri
PENDING→AVAILABLE → verifikasi dengan `GetBalance`. Singapay tidak menyediakan artefak
yang setara dengan CSV itu.

### 6.1 Yang tersedia

**a. Webhook `settlement.completed`** — level batch, bukan level transaksi:

```json
{ "event": "settlement.completed", "timestamp": "18 Jun 2026 10:00:00",
  "data": { "settlement": {
      "id": 1234, "reference_no": "SETTLEMENT-1-ABC123",
      "status": "completed", "settlement_type": "ALL",
      "settlement_method": "balance",         // balance | auto-balance | bank-account | e-wallet
      "is_auto_created": false,
      "start_date": "01 Jun 2026 00:00:00", "end_date": "17 Jun 2026 23:59:59",
      "amount": 1000000,                      // total NET
      "total_admin_fee": 5000, "total_vendor_fee": 3000, "total_our_margin": 2000,
      "settlement_fee": 0, "total_to_transfer": 1000000, "total_refunded": 0,
      "approved_by": "Jane Finance", "approved_at": "18 Jun 2026 10:00:00" },
    "total_transactions": 5 } }
```

Padanan `DokuSettlementCSVMetadata` (`BatchID` → `reference_no`, `TotalFee` →
`total_admin_fee + total_vendor_fee + total_our_margin`, `TotalTransactions` →
`total_transactions`). **Tapi tidak ada daftar transaksinya.**

**b. List transaksi per akun per produk**, difilter `settle_at_from` / `settle_at_to`
sesuai rentang batch. Inilah pengganti baris-baris CSV. Butuh N panggilan per akun per
produk, dengan paginasi.

**c. `GET /api/v1.0/statements/{account_id}`** — ledger per akun, `debit`/`credit`/
`balance_after` per baris dengan `merchant_reff_no`. Difilter `processed_timestamp`.

**d. Webhook `settlement.refunded` / `settlement.refund_cancelled`** — satu transaksi
ditarik balik dari saldo setelah settlement. **Tidak ada padanannya di alur DOKU sekarang
sama sekali**, dan ini menyentuh uang yang sudah AVAILABLE dan mungkin sudah ditarik
seller. Perlu jenis entri ledger baru (kemungkinan sejalan dengan `FEE_ADJUSTMENT` di
`104-fee-mismatch-reconciliation.md`) dan kebijakan untuk saldo negatif.

### 6.2 Bentuk alur pengganti

```
webhook settlement.completed (batch B, rentang [start,end], method=balance)
  → simpan settlement_batch (batch_id = reference_no)   // unique index yang ada tetap dipakai
  → untuk setiap akun yang punya transaksi PENDING:
      GET /va-transactions/{acc}?settle_at_from=start&settle_at_to=end   (paginasi)
      GET /qris-dynamic/{acc}?...
      GET /ewallet-native-transactions/{acc}?...
      GET /payment-link-histories/{acc}?settle_at_from=...&settle_at_to=...
  → cocokkan merchant_reff_no ↔ product_transactions.invoice_number
  → tulis entri PENDING→AVAILABLE, dan FEE_ADJUSTMENT dari fee aktual per transaksi
  → verifikasi: GET /balance-inquiry/{acc} vs GetAllBalances()
```

Yang tetap bisa dipakai apa adanya: `SettlementBatch`, `SettlementItem`,
`ReconciliationDiscrepancy`, idempotensi lewat `batch_id`, dan seluruh logika penulisan
entri ledger. Yang harus diganti: `DokuSettlementCSVParser`, `FilterIngestedReportFiles`
(tidak ada lagi konsep nama file), dan `ReconciliationRequest.CSVReader`.

Sesuai anjuran Singapay sendiri: *"Never rely solely on webhook events for reconciliation
— always back them up with a daily scheduled pull of the transaction history API."*
Job harian tetap diperlukan.

---

## 7. Model fee

`FeeModelGatewayOnCustomer` dan `FeeModelGatewayOnSeller` di repo ini dihitung **lokal**
oleh `domain.FeeCalculator` dari tabel `fee_configs`, lalu hasilnya dikirim sebagai
`total_amount`. Cara itu tetap bekerja di Singapay tanpa perubahan konsep — yang perlu
diganti hanyalah isi `fee_configs` (kode channel dan besaran fee Singapay).

Dua hal yang perlu diketahui:

- **Default Singapay adalah fee ditanggung merchant.** Webhook VA menunjukkan
  `amount: 100000` dengan `fees.amount: 1500` — merchant menerima 98.500. Jadi
  `GATEWAY_ON_CUSTOMER` tetap harus diwujudkan dengan menaikkan `total_amount` seperti
  sekarang, bukan dengan flag di gateway.
- **`customer_pays_fee` tidak bisa diatur lewat API.** Field itu ada di respons Payment
  Link tapi tidak ada di `CreatePaymentLinkRequest` maupun `CreatePaymentLinkV2Request`,
  dan dokumentasinya menyatakan "Always `false` for links created via v2". Pengaturannya
  hanya lewat dashboard. Jangan andalkan itu.

Tidak ada endpoint kuotasi fee untuk money-in (hanya untuk disbursement). Jadi
`CalculateFeesForCustomer` tetap sepenuhnya berbasis konfigurasi lokal, dengan risiko
drift yang sama seperti sekarang — yang justru membuat rekonsiliasi fee aktual
(`104-fee-mismatch-reconciliation.md`) makin penting, bukan makin tidak penting.

---

## 8. Fitur Singapay yang tidak punya padanan di DOKU

Bukan kebutuhan migrasi, tapi berguna diketahui:

- **Subscription/recurring native** (`/api/v2.0/recurring/plans`) berbasis tokenisasi
  kartu, dengan retry policy dan webhook per siklus. `GenerateSubscriptionPayment` di repo
  ini sekarang cuma pembayaran satu kali berlabel `SUBSCRIPTION`.
- **QRIS money-out** dan **e-wallet money-out** — payout ke wallet/QRIS merchant.
- **Direct Debit** (SOON) — tarik dana dari rekening bank dengan binding sekali.
- **Cardless withdrawal** (SOON) — tarik tunai di ATM dengan OTP.
- **KYB self-onboarding** untuk sub-account `business_managed`, lengkap dengan
  `kyb_onboarding_url` dan webhook hasilnya. Relevan kalau suatu saat seller harus tampil
  sebagai merchant sendiri ke pembeli.
- **Statement API per akun** — DOKU tidak punya; ini alat audit yang cukup berharga.

---

## 9. Gap, risiko, dan ambiguitas dokumentasi

### 9.1 Rekonsiliasi kehilangan artefak tunggalnya
Tidak ada CSV settlement platform-wide. Penggantinya N panggilan API berpaginasi per akun
per produk. Untuk platform dengan banyak seller, ini berbeda ordo besaran dalam biaya
maupun kompleksitas. **Tanyakan apakah ada endpoint atau export settlement detail per
batch** — kalau ada dan tidak terdokumentasi, ini menghemat pekerjaan paling banyak.

### 9.2 Payment Link tidak mengekspos fee per transaksi
`104-fee-mismatch-reconciliation.md` bergantung pada `ActualDokuFee` per transaksi. VA,
QRIS, dan e-wallet menyediakannya; Payment Link History tidak (tidak ada `fees`,
`mdr_cost`, maupun `net_amount` di skemanya). Kalau Payment Link jadi jalur bayar utama,
rekonsiliasi fee kehilangan sumber datanya. **Ini alasan kuat memakai endpoint produk
langsung (VA/QRIS) alih-alih Payment Link** bila channel sudah ditentukan di muka.

### 9.3 Pembuatan sub-account tidak idempoten
Lihat §5.1. Retry setelah timeout bisa menciptakan sub-account kedua yang menampung uang.

### 9.4 `X-Timestamp`: detik atau ISO-8601?
Guide bilang Unix detik; OpenAPI spec disbursement bilang ISO-8601 (§3.2). Harus dites di
sandbox sebelum menulis client.

### 9.5 Saldo bertipe string desimal
`"1234.56"` vs `int64` di ledger. `fmt.Sscanf("%d")` yang ada sekarang akan memotong
diam-diam. Lihat §5.6.

### 9.6 Amount bertipe tidak konsisten antar payload
`"10000.00"` (string) di webhook Payment Link, `100000` (number) di webhook VA,
`50000` (number) di request disbursement, `"500000"` (string) di respons account transfer.
Client harus toleran di kedua arah, seperti `FlexibleInt` yang sudah ada.

### 9.7 `kind` di statement cuma dua nilai
Skema statement membatasi `kind` ke `virtual-account` dan `disbursement`, padahal guide
menyebut pembayaran QRIS, kartu, e-wallet, fee, settlement, dan refund semuanya muncul di
statement. Salah satu dari keduanya sudah usang. Jangan bangun rekonsiliasi di atas
`kind` sebelum dikonfirmasi.

### 9.8 Webhook Payment Link tidak punya `event` di contohnya
Halaman *Shared Webhook Endpoints* menyuruh routing berdasarkan `event`, dan menyebut
`payment-link-transaction` sebagai nilainya. Tapi contoh payload di halaman webhook
Payment Link **tidak memuat field `event`** (berbeda dari contoh VA yang memuatnya).
Handler harus punya fallback: kalau `event` tidak ada, deteksi dari
`data.payment.method`. Konfirmasi ke Singapay.

### 9.9 Tidak ada daftar bank via API
Lihat §5.8.

### 9.10 IP whitelist wajib
Kegagalan di produksi akan muncul sebagai `SP017`, bukan sebagai error jaringan. Masukkan
ke checklist deploy, termasuk IP outbound dari semua worker.

### 9.11 `check-fee` hanya menerima SWIFT, `transfer` v2 menerima keduanya
`POST /api/v1.0/disbursement/{account_id}/check-fee` memakai field `bank_swift_code` dan
tidak punya versi v2, sementara `POST /api/v2.0/disbursement/transfer` memakai `bank_code`
yang menerima sandi bank 3 digit **atau** SWIFT. Kalau `Disbursement.BankAccount.BankCode`
diisi sandi 3 digit (yang wajar, karena `check-beneficiary` v2 menerimanya), alur "cek fee
lalu transfer" patah di langkah pertama tanpa alasan yang jelas dari pesan errornya.
Simpan SWIFT sebagai kanonik, atau simpan keduanya.

### 9.12 Refund pasca-settlement
`settlement.refunded` bisa menarik dana yang sudah AVAILABLE dan mungkin sudah ditarik
seller. Perlu kebijakan saldo negatif yang saat ini tidak ada di ledger.

---

## 10. Pertanyaan untuk tim Singapay

1. Apakah ada endpoint/export **detail settlement per batch** (daftar transaksi dalam satu
   `reference_no`)? Ini penentu terbesar bentuk rekonsiliasi.
2. Apakah `POST /api/v1.0/accounts` mendukung **idempotency key** atau menolak nama/
   referensi duplikat?
3. `X-Timestamp` untuk `POST /api/v2.0/disbursement/transfer`: **Unix detik atau ISO-8601**?
4. Bisakah **fee per transaksi Payment Link** diekspos (di history atau webhook)?
5. Apa definisi `held_balance`, dan apakah ia termasuk dalam `balance`?
6. Adakah **daftar bank + kode + limit** yang bisa diambil via API?
7. Webhook Payment Link: apakah field `event` selalu dikirim?
8. Apakah `merchant_reff_no` dijamin unik dan bisa dipakai sebagai kunci pencarian di
   semua endpoint list money-in?
9. Berapa **rate limit** API, terutama untuk polling list transaksi harian per sub-account?
10. Apakah sub-account `owned` bisa dipakai untuk menerima pembayaran **dan** melakukan
    disbursement langsung dari saldonya sendiri? (Skema mengindikasikan ya — perlu
    konfirmasi tertulis karena inilah fondasi seluruh model ledger repo ini.)
11. Apakah Singapay menerima pemakaian `owned` untuk seller pihak ketiga yang dibuat
    otomatis tanpa KYB per seller, mengingat `business_managed` (yang butuh approval
    manual) mustahil dipakai di jalur booking sinkron? Ini pertanyaan kepatuhan, dan
    jauh lebih murah dijawab sebelum ribuan sub-account terbentuk.

---

## 11. Urutan kerja yang disarankan

Bertahap, tiap tahap bisa diuji sendiri:

1. **Client Singapay** — ✅ **sudah ada di [`singapay/`](../singapay/)**: token (dengan
   cache), tiga skema signature, verifikasi webhook, `Amount` yang menolak memotong
   desimal diam-diam, dan `Outcome` yang memetakan SP-code ke "boleh lepas reservasi atau
   tidak". Mencakup accounts, balance, account transfer, dan disbursement (termasuk
   check-fee, check-beneficiary, inquiry-status). Murni permukaan API — tidak menyentuh
   domain ledger.

   Money-in juga sudah lengkap: Payment Link, Virtual Account, QRIS, e-wallet, plus
   parser webhook untuk keempat channel money-in, disbursement, dan settlement.
   `MoneyInNotification.MerchantReference()` menyembunyikan aturan per-channel yang
   berbeda-beda (§5.3).

   Ada juga [`cmd/singapay-smoke`](../cmd/singapay-smoke/) untuk memverifikasi ke
   sandbox: `-step readonly` membuktikan kredensial dan IP allowlist, `-step signature`
   menyelesaikan §9.4 dengan mencoba kedua format sekaligus, dan `-step va|qris|
   payment-link` menerbitkan instrumen bayar sungguhan untuk memancing webhook.

   **Belum diverifikasi terhadap server sungguhan.**
2. **Accounts + balance** — `CreateAccount`, simpan ULID **dan** `account_number`. Migrasi
   skema: `doku_sub_account_id` → `gateway_account_id` + `gateway_account_number`.
3. **Bank inquiry** — paling terisolasi, tidak menyentuh uang.
4. **Disbursement** — `check-fee` → reservasi **nominal permintaan** (fee dipotong dari
   dalamnya, lihat migrasi 028) → `transfer` sebesar net → pemetaan status
   `00/01/02/03/04/05/06/07` → handler webhook `disbursement` → `SP004` menuju
   `inquiry-status`.
5. **Account transfer** untuk platform fee — `merchant_ref_no` sebagai idempotency key.
6. **Payment** — mulai dari VA/QRIS langsung (fee per transaksi tersedia), Payment Link
   hanya bila channel memang dibiarkan dipilih pembeli.
7. **Webhook money-in** — routing `event` + fallback, dua jalur ekstraksi invoice.
8. **Rekonsiliasi** — paling akhir, dan hanya setelah §10.1 terjawab.

Selama transisi, `LedgerClient` sebaiknya menyimpan **nama gateway per akun**
(`gateway: "doku" | "singapay"`) sehingga seller lama tetap bisa ditarik dananya dari DOKU
sementara seller baru sudah di Singapay. Big-bang cutover berarti saldo yang masih ada di
sub-account DOKU harus dipindah lebih dulu — dan itu pekerjaan tersendiri.

---

## 12. Referensi

- Overview: https://docs.singapay.id/guides/en/getting-started/overview
- Indeks lengkap dokumentasi: https://docs.singapay.id/llms.txt
- **OpenAPI merchant API** (paling akurat): https://payment-b2b.singapay.id/api/docs/merchant-api.json
- Authentication: https://docs.singapay.id/api-reference/authentication
- Building X-Signature: https://docs.singapay.id/api-reference/building-x-signature
- Webhook signature: https://docs.singapay.id/api-reference/webhooks/security-and-signature
- Shared webhook endpoints: https://docs.singapay.id/api-reference/webhooks/shared-endpoints
- Response codes (SP000–SP020): https://docs.singapay.id/api-reference/references/response-codes
- Transaction status (00–07): https://docs.singapay.id/api-reference/references/transaction-status
- Settlement webhook: https://docs.singapay.id/api-reference/webhooks/settlement
- Reconciliation guide: https://docs.singapay.id/guides/en/reports/reconciliation
