# Kontrak API

Versi `v1`. Semua path diawali `/v1` kecuali probe kesehatan.

Dokumen ini menggambarkan apa yang **benar-benar dijawab server**, bukan apa
yang seharusnya. Bentuk respons di bawah disalin dari permintaan sungguhan, dan
setiap status code di dalamnya diverifikasi terhadap server yang berjalan pada
2026-08-10.

---

## 1. Model keamanan

Ada **dua kredensial**, dan keduanya tidak saling menggantikan.

| | Milik | Dikirim lewat | Untuk |
|---|---|---|---|
| **API key** | operator / integrator | `X-API-Key` | membuka sesi, seluruh galeri |
| **Session nonce** | subjek yang diverifikasi | `X-Session-Nonce` | mengoperasikan **satu** sesi |

**API key tidak boleh sampai ke browser subjek.** Ia mengizinkan pembuatan sesi
tanpa batas dan penulisan ke galeri. Yang dipegang browser adalah `session_id`
dan `nonce` — masing-masing 128 bit acak, kedaluwarsa bersama sesinya, dan tidak
bisa apa-apa di luar sesi itu.

Nonce dibandingkan dalam waktu konstan, dan diperiksa **sebelum** sesi disentuh.
Nonce yang salah dijawab `403` dan tidak mengubah apa pun — mengetahui sebuah
`session_id` tidak cukup untuk merusak verifikasi orang lain.

### Rate limit

Token bucket per alamat IP klien, `LV_RATE_LIMIT_PER_MIN` (default 600).
Header `X-Forwarded-For` **tidak dipercaya**: ia sepele dipalsukan, dan
mempercayainya mengubah limiter jadi cara menghabiskan jatah orang lain.

---

## 2. Alur yang dimaksudkan

```
backend Anda                    service ini                 browser subjek
     │                              │                              │
  1  ├── POST /v1/liveness/sessions ►│   X-API-Key                  │
     │◄─ session_id, nonce, challenges                              │
     │                              │                              │
  2  ├── serahkan id + nonce ───────┼─────────────────────────────►│
     │                              │                              │
  3  │                              │◄─ POST .../frames ───────────┤  ~6 fps
     │                              │─► challenge berikutnya,      │
     │                              │   sisa detik, alasan         │
     │                              │      (diulang sampai selesai)│
     │                              │                              │
  4  │                              │◄─ POST .../complete ─────────┤
     │◄─ token ─────────────────────┼──────────────────────────────┤
     │                              │                              │
  5  ├── POST /v1/faces ───────────►│   X-API-Key + token          │
     │                              │   wajah dicocokkan dengan    │
     │                              │   capture yang terverifikasi │
     │◄─ face_id ───────────────────┤                              │
```

**Langkah 5 bukan formalitas.** Token membuktikan *sebuah sesi* lolos — ia tidak
mengatakan apa pun tentang wajah siapa yang datang bersamanya. Karena itu wajah
yang dikirim dicocokkan kembali dengan embedding acuan sesi yang menerbitkan
token itu. Tanpa pencocokan itu, seseorang bisa lolos liveness dengan wajahnya
sendiri lalu mendaftarkan foto orang lain.

---

## 3. Bentuk error

Setiap error memakai amplop yang sama:

```json
{
  "error": {
    "code": "unauthorized",
    "message": "a valid X-API-Key header is required",
    "request_id": "9f2c1a7b3e4d5c60"
  }
}
```

| `code` | HTTP | Arti |
|---|---|---|
| `bad_request` | 400 | Permintaan tidak berbentuk |
| `unauthorized` | 401 | API key tidak ada atau salah |
| `forbidden` | 403 | Nonce salah, atau token liveness tidak sah |
| `not_found` | 404 | Sesi, subjek, atau endpoint tidak ada |
| `method_not_allowed` | 405 | |
| `conflict` | 409 | Sesi sudah selesai, atau challenge belum tuntas |
| `gone` | 410 | Sesi kedaluwarsa |
| `payload_too_large` | 413 | Frame lebih besar dari batas |
| `bad_request` | 415 | Bukan JPEG atau PNG (status 415, kode tetap `bad_request`) |
| `unprocessable_entity` | 422 | **Verifikasi gagal** |
| `rate_limited` | 429 | |
| `internal_error` | 500 | |
| `service_unavailable` | 503 | Hanya dari `/readyz` |

### Kenapa `422` tidak pernah menjelaskan dirinya

Setiap pertahanan yang gagal — replay urutan, gambar diam, spoof terdeteksi,
identitas berubah — menghasilkan `422` dengan pesan yang **sama persis**:
`"verification failed"`.

Ini disengaja. Klien yang bisa membedakan "cek identitas yang menolak" dari
"cek spoof yang menolak" belajar pertahanan mana yang harus diakali berikutnya.
Alasan sebenarnya masuk ke log server, di balik kendali akses.

Ada test yang menjalankan dua serangan berbeda dan menuntut keduanya
menghasilkan respons yang identik.

---

## 4. Probe kesehatan

Keduanya tanpa autentikasi: load balancer tidak bisa memegang kredensial.

### `GET /healthz`

Menjawab **"proses ini hidup"**. Selalu `200` selama proses berjalan.

```json
{"status":"ok","pipeline":"onnx"}
```

### `GET /readyz`

Menjawab **"proses ini bisa melayani"**. Memeriksa database, versi skema, dan
object store bila retensi menyala.

```json
{"status":"ready"}
```

Bila ada yang gagal — `503`:

```json
{"status":"not ready","failing":["database"]}
```

Hanya **nama** cek yang gagal yang disebut, tidak pernah alasannya. Endpoint ini
dibaca semua orang; alasannya masuk ke log.

> Sebuah proses bisa sangat hidup dengan database yang tidak terjangkau.
> Menjawab probe dengan `200` dalam keadaan itu adalah cara instance rusak terus
> menerima trafik.

---

## 5. Liveness

### `POST /v1/liveness/sessions`

Membuka sesi. **Auth:** `X-API-Key`
(kecuali `LV_ALLOW_ANONYMOUS_SESSIONS=true`, yang hanya untuk demo).

Tanpa body.

**`201 Created`**

```json
{
  "session_id": "efd7a9a1fcbf0b374aed0745fe04b643",
  "nonce": "126563af0d68b7281c64dd34d47218b8",
  "challenges": ["MOUTH_OPEN", "TURN_RIGHT", "NOD"],
  "challenge_seconds": 5,
  "seconds_remaining": 4.942704417,
  "expires_at": "2026-08-10T03:14:35.815751225Z",
  "challenge_deadline": "2026-08-10T03:13:10.815751225Z"
}
```

`challenges` diurutkan acak per sesi, dari `BLINK`, `TURN_LEFT`, `TURN_RIGHT`,
`NOD`, `MOUTH_OPEN`. Ketidakterdugaan itulah pertahanan terhadap rekaman — dan
itu tetap berlaku meski klien tahu urutannya, karena penyerang harus menyiapkan
rekamannya **setelah** sesi dimulai dan di dalam 90 detiknya.

> Arah pada `TURN_LEFT` / `TURN_RIGHT` relatif terhadap **gambar**, bukan
> subjek. Preview selfie dicerminkan, jadi klien harus membalik instruksinya:
> `TURN_LEFT` ditampilkan sebagai "tengok ke kanan Anda".

---

### `POST /v1/liveness/sessions/{id}/frames`

Mengirim satu frame. **Auth:** `X-Session-Nonce`

```json
{
  "seq": 1,
  "nonce": "126563af0d68b7281c64dd34d47218b8",
  "frame": "data:image/jpeg;base64,/9j/4AAQ..."
}
```

| Field | Aturan |
|---|---|
| `seq` | Bilangan positif, **naik ketat**. Mundur atau berulang → sesi gagal. |
| `nonce` | Sama dengan header. |
| `frame` | JPEG atau PNG, base64. Prefiks data URL diterima. |

Kirim sekitar **6 frame per detik**. Lebih cepat hanya menumpuk antrean —
anggaran terukur 149 ms per frame.

> **Kirim frame pada sisi terpanjang minimal 720 px.** Gerbang kualitas menolak
> wajah yang lebih sempit dari 112 px (lebar input ArcFace), dan pada frame 480
> px wajah pada jarak laptop biasa terukur 105–111 px — **setiap frame ditolak
> sebelum satu pun challenge dievaluasi**.

**`200 OK`**

```json
{
  "state": "IN_PROGRESS",
  "challenge": "TURN_RIGHT",
  "advanced": true,
  "completed": false,
  "remaining": 2,
  "retried": false,
  "retries_left": 2,
  "seconds_remaining": 4.87,
  "reason": ""
}
```

| Field | Arti |
|---|---|
| `challenge` | Yang **sedang** diminta. Berubah saat `advanced` bernilai true. |
| `advanced` | Challenge sebelumnya terpenuhi. |
| `completed` | Semua challenge selesai — panggil `/complete`. |
| `retried` | Challenge kehabisan waktu dan **diulang**, bukan menggagalkan sesi. |
| `retries_left` | Sisa pengulangan untuk seluruh sesi. |
| `seconds_remaining` | Sisa detik challenge saat ini. |
| `reason` | Apa yang kurang, untuk ditampilkan ke subjek. Kosong saat maju. |

**Respons tidak pernah memuat skor, landmark, atau embedding.** Klien yang bisa
melihat seberapa dekat sebuah frame dengan ambang spoof bisa menyetel serangan
satu permintaan demi satu permintaan.

`reason` yang mungkin muncul: `"no face in view"`, `"move closer to the
camera"`, `"hold steady and make sure your face is well lit"`, `"blink"`,
`"keep going, now open your eyes"`, `"turn further"`, `"nod further"`,
`"open your mouth wider"`.

**Frame yang tidak terpakai bukan kegagalan.** Wajah tidak terlihat, terlalu
jauh, atau buram → `200` dengan `reason`; kirim frame berikutnya. Hanya replay,
spoof, dan pergantian wajah yang mengakhiri sesi.

---

### `POST /v1/liveness/sessions/{id}/complete`

Meminta putusan. **Auth:** `X-Session-Nonce`. Tanpa body.

**`200 OK`**

```json
{
  "session_id": "efd7a9a1fcbf0b374aed0745fe04b643",
  "state": "PASSED",
  "passed": true,
  "token": "kR8f2wQ7vN3mZ1xY..."
}
```

`token` **hanya ada bila `passed` bernilai true**, dan hanya dikirim sekali —
yang disimpan server adalah HMAC-nya. Sekali pakai, berumur pendek
(`LV_TOKEN_SECRET` + `LV_TOKEN_TTL`, default 5 menit), terikat pada satu sesi.

`409` bila masih ada challenge tersisa. `410` bila sesi kedaluwarsa.

---

### `GET /v1/liveness/sessions/{id}`

Status sesi. **Auth:** `X-Session-Nonce`

```json
{
  "session_id": "efd7...",
  "state": "IN_PROGRESS",
  "challenge": "NOD",
  "challenges": ["MOUTH_OPEN", "TURN_RIGHT", "NOD"],
  "remaining": 1,
  "seconds_remaining": 3.2,
  "completed": false,
  "expires_at": "2026-08-10T03:14:35Z"
}
```

Membaca status **tidak** menghabiskan kesempatan pengulangan dan tidak
mengakhiri sesi yang masih bisa dilanjutkan.

---

## 6. Galeri wajah

Ketiganya menuntut `X-API-Key`. Token liveness membuktikan capture-nya hidup;
API key membuktikan siapa yang berhak menulis ke galeri. Keduanya tidak saling
menggantikan.

### `POST /v1/faces`

```json
{
  "token": "kR8f2wQ7vN3mZ1xY...",
  "subject_id": "pelanggan-88213",
  "image": "data:image/jpeg;base64,/9j/4AAQ..."
}
```

**`201 Created`**

```json
{
  "face_id": "9f2c1ab7e4d3...",
  "subject_id": "pelanggan-88213",
  "session_id": "efd7a9a1..."
}
```

| Kegagalan | HTTP |
|---|---|
| `token` atau `subject_id` kosong | `400` |
| `image` bukan JPEG/PNG yang bisa dibaca | `415` |
| Token tidak ada, kedaluwarsa, atau **sudah terpakai** | `403` |
| Sesi di balik token sudah tidak terbaca | `410` |
| Wajah tidak cocok dengan capture yang terverifikasi | `422` |
| Wajah tidak terdeteksi atau kualitas kurang | `422` |

#### Urutan pemeriksaan

Dan urutannya berpengaruh pada apa yang Anda terima:

```
1. bentuk JSON, field wajib   → 400
2. format gambar              → 415
3. token ditebus (SEKALI PAKAI) → 403
4. wajah dicocokkan            → 422
```

**Gambar yang rusak dijawab `415` sebelum token disentuh**, jadi kesalahan
encoding di sisi Anda tidak membakar token yang sah — klien bisa mencoba lagi
dengan token yang sama.

Begitu langkah 3 lewat, token **habis**, apa pun yang terjadi sesudahnya. Wajah
yang tidak cocok (`422`) berarti subjek harus memverifikasi ulang dari awal.
Itu disengaja: token yang selamat dari kegagalan adalah token yang bisa dicoba
terus sampai ada yang tembus.

> **Token dibelanjakan lebih dulu, sebelum pekerjaannya.** Permintaan yang gagal
> kemudian tidak bisa diulang dengan token yang sama. Token yang selamat dari
> kegagalan adalah token yang bisa dicoba terus sampai ada yang tembus.

Satu subjek boleh punya beberapa wajah. Mendaftarkan orang yang sama dari
beberapa capture membuat pencarian tahan pose dan cahaya.

---

### `POST /v1/faces/search`

```json
{ "image": "data:image/jpeg;base64,/9j/4AAQ..." }
```

**`200 OK`**

```json
{
  "matched": true,
  "best":  { "face_id": "9f2c...", "subject_id": "pelanggan-88213", "score": 0.91 },
  "candidates": [
    { "face_id": "9f2c...", "subject_id": "pelanggan-88213", "score": 0.91 },
    { "face_id": "3a1b...", "subject_id": "pelanggan-40117", "score": 0.31 }
  ]
}
```

`matched` berarti kandidat teratas melewati `LV_ENROLL_MATCH_COSINE_MIN`.
`candidates` disertakan supaya operator bisa melihat nyaris-cocok — itulah yang
membuat ambangnya bisa disetel.

> Skor **disertakan di sini** meski jalur liveness menyembunyikannya. Bedanya
> ada pada apa yang dibeli angka itu: skor liveness memberi tahu penyerang
> seberapa dekat mereka dengan menembus pertahanan, sedangkan kemiripan galeri
> adalah jawaban yang memang ditanyakan pemanggil.

`404` **`"the gallery is empty"`** bila belum ada wajah terdaftar sama sekali.
Ini berbeda dari `200` dengan `matched: false` — galeri kosong dan tidak ada
yang cocok adalah dua persoalan operasional yang berbeda.

---

### `DELETE /v1/faces`

```json
{ "subject_id": "pelanggan-88213" }
```

**`200 OK`** — `{"faces_removed": 3}` · **`404`** bila subjek tidak punya wajah.

> **`subject_id` ada di body, bukan di path.** Segmen path terbaca sebagai
> pilihan RESTful dan justru salah di sini: `subject_id` ditentukan integrator
> dan lazimnya nomor identitas atau nomor rekening, sementara path mendarat di
> log akses, log proxy, dan riwayat peramban.

Penghapusan menghapus **barisnya**, bukan menandainya. Template biometrik yang
masih terbaca belum terhapus. Jejak auditnya ada di tabel terpisah dan hidup
lebih lama daripada yang dihapus.

---

## 7. Batasan yang memengaruhi integrasi

- **Anti-spoof pasif sedang dimatikan.** Foto cetak dan replay layar **tidak
  diblokir**. Yang tersisa adalah liveness aktif. Jangan menyebut sistem ini
  PAD-compliant selama `LV_LIVENESS_ANTISPOOF_ENFORCE=false`.
- **Ambang belum terkalibrasi.** Sebagian berasal dari satu sesi dengan satu
  orang. Jalankan `calibrate` sebelum mempercayainya untuk keputusan serius.
- **`subject_id` dipercaya apa adanya.** Service ini tidak punya pendapat
  tentang siapa berhak memakai identitas mana — itu urusan backend Anda.
- **Demo bawaan hanya jalan di `localhost`.** `getUserMedia` menuntut secure
  context.
- **Single-node, single-tenant.**

---

## 8. Contoh alur lengkap

```bash
KEY=your-api-key
BASE=http://localhost:8080

# 1. buka sesi
S=$(curl -s -X POST $BASE/v1/liveness/sessions -H "X-API-Key: $KEY")
ID=$(echo $S | jq -r .session_id)
N=$(echo $S | jq -r .nonce)

# 2. kirim frame, ~6 per detik, sampai completed bernilai true
curl -s -X POST $BASE/v1/liveness/sessions/$ID/frames \
  -H "X-Session-Nonce: $N" -H 'Content-Type: application/json' \
  -d "{\"seq\":1,\"nonce\":\"$N\",\"frame\":\"$(base64 -w0 frame.jpg)\"}"

# 3. minta putusan; token hanya keluar bila lolos
TOKEN=$(curl -s -X POST $BASE/v1/liveness/sessions/$ID/complete \
  -H "X-Session-Nonce: $N" | jq -r .token)

# 4. daftarkan wajah dengan token itu
curl -s -X POST $BASE/v1/faces -H "X-API-Key: $KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"token\":\"$TOKEN\",\"subject_id\":\"pelanggan-88213\",\"image\":\"$(base64 -w0 face.jpg)\"}"
```
