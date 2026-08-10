# Spec: Liveness Verifier

> Status: **DISETUJUI — mengikuti implementasi** · Versi 0.3 · 2026-08-10
> Fase spec-driven: **4/4 (Implement)**. 31 task selesai. Tiga kriteria di §9
> masih merah — **A5** (anti-spoof) menunggu jawaban di §10, **X2** (coverage)
> dan **X6** (README di mesin bersih) menunggu kerja.
>
> **Kepala dokumen ini sempat berbohong.** Sampai versi 0.3 ia masih menulis
> "DRAFT · Versi 0.1 · belum boleh masuk Plan" sementara riwayat revisinya
> sendiri mencatat persetujuan di 0.2 dan seluruh implementasinya sudah jadi.
> Dicatat di sini, bukan diam-diam diperbaiki: dokumen yang salah tentang
> statusnya sendiri adalah dokumen yang orang berhenti percayai.

---

## 1. Objective

Membangun **satu service Golang** yang berjalan sepenuhnya lokal (offline, via Docker Compose) untuk:

1. **Active liveness verification** — memastikan yang ada di depan kamera adalah manusia hidup, bukan foto cetak, replay video, atau mask, dengan metode *challenge-response* (kedip, tengok kiri/kanan, buka mulut).
2. **Face enrollment + 1:N identification** — mendaftarkan wajah ke galeri dan mencari identitas dari galeri tersebut berdasarkan satu foto/selfie.

### Siapa penggunanya

| Persona | Kebutuhan |
|---|---|
| **Developer (Anda)** | Menjalankan `docker compose up`, membuka demo web, mencoba flow end-to-end di laptop tanpa dependency cloud. |
| **Sistem pemanggil (integrator)** | REST API JSON yang stabil untuk memulai sesi liveness, mengirim frame, dan mengambil verdict + hasil pencarian identitas. |
| **Auditor / operator** | Bisa menelusuri kembali keputusan sistem: sesi mana, kapan, skor berapa, frame apa yang dipakai. |

### Apa yang **bukan** tujuan project ini

- Bukan sistem tersertifikasi **ISO/IEC 30107-3 (PAD)**. Ini development/riset build, bukan sistem produksi eKYC yang lolos audit.
- Bukan multi-tenant SaaS. Single-tenant, single-node, jalan di satu mesin.
- Tidak melatih model dari nol. Kita memakai model ONNX pre-trained.

### ⚠️ Catatan penting sebelum lanjut

**Scope 1:N search adalah bagian terberat dari spec ini.** Active liveness sendiri sudah merupakan sistem lengkap (state machine sesi, anti-replay, 4 model inference). Menambahkan enrollment + 1:N search menggandakan permukaan: butuh vector store, index tuning, kalibrasi threshold, dan lifecycle data biometrik (hapus, update, retensi).

Rekomendasi saya: **kerjakan berurutan** — Milestone A (liveness) harus jalan dan terverifikasi dulu, baru Milestone B (enrollment/1:N). Spec ini menuliskan keduanya secara penuh, tapi Plan di fase berikutnya akan memisahkannya. Kalau Anda ingin memangkas scope, potong di Milestone B.

**Lisensi model:** model pre-trained InsightFace (SCRFD, ArcFace/buffalo_l) dirilis untuk **penggunaan riset non-komersial**. Untuk pemakaian komersial Anda perlu model lain atau lisensi terpisah. Ini di luar kendali kode — lihat §9 Open Questions.

---

## 2. Tech Stack

| Lapisan | Pilihan | Alasan |
|---|---|---|
| Bahasa | **Go 1.23**, CGO **enabled** | Wajib CGO untuk binding ONNX Runtime. |
| HTTP | `net/http` + `github.com/go-chi/chi/v5` | Router ringan, middleware ecosystem (request ID, recover, timeout) tanpa framework berat. |
| Inference | `github.com/yalue/onnxruntime_go` v1.32 + **ONNX Runtime 1.28.0 (CPU EP)** | Satu container, satu binary. Tidak perlu sidecar Python. **Kedua versi bergerak bersama:** binding Go menyematkan header C dengan `ORT_API_VERSION` tetap dan menolak inisialisasi terhadap library yang lebih lama. API version melacak minor version ORT — 1.28 → API 28. Menaikkan salah satunya berarti memeriksa yang lain. |
| Image I/O | `image/jpeg`, `image/png`, `golang.org/x/image/draw` | Stdlib cukup; hindari dependency OpenCV/gocv yang memberatkan build. |
| Linear algebra | `gonum.org/v1/gonum` | PnP solver untuk head pose, operasi vektor embedding. |
| Database | **PostgreSQL 16 + pgvector 0.7** (`pgvector/pgvector:pg16`) | Audit trail relasional + vector index (HNSW) dalam satu engine. Tidak perlu vector DB terpisah. |
| DB driver | `github.com/jackc/pgx/v5` (pool) | Native protocol, dukungan tipe `vector` lewat custom codec. |
| Migrasi | `github.com/pressly/goose/v3` | SQL migration eksplisit, embedded ke binary. |
| Object storage | **MinIO** (`minio/minio`) + `github.com/minio/minio-go/v7` | S3-compatible, simpan artefak frame/foto enrollment. |
| Session store | Tabel Postgres `liveness_sessions` (TTL via `expires_at`) | Menghindari container Redis tambahan. Beban sesi lokal rendah. |
| Logging | `log/slog` (stdlib), JSON handler | Zero dependency, structured. |
| Config | Env var → struct `internal/config`, di-load sekali saat boot | 12-factor. Tidak ada file config runtime. |
| Demo UI | HTML + vanilla JS + `getUserMedia`, di-`embed` ke binary | Tidak ada Node/npm. "Project tunggal" tetap tunggal. |
| Test | `testing` stdlib saja | **Tanpa testify:** assertion tiga baris tidak sepadan dengan satu dependensi lagi di jalur yang menguji data biometrik. **Tanpa testcontainers:** ia menuntut socket Docker di-mount ke dalam proses test, dan memberi test kemampuan menjalankan container sembarang adalah hak yang tidak dibutuhkan — Postgres dan MinIO yang diinginkannya sudah hidup di jaringan compose yang sama. |
| Lint | `golangci-lint` v1.61, `gofumpt` | Gate kualitas di CI dan pre-commit. |

### Model ONNX yang dipakai

| Peran | Model | Output | Ukuran |
|---|---|---|---|
| Face detection | **SCRFD-500M** (`det_500m.onnx`) | bbox + 5 keypoint | 2,4 MB |
| Dense landmark | **2d106det** (`2d106det.onnx`) | 106 titik 2D | 4,8 MB |
| Head pose | Turunan dari 106 landmark via **PnP** (gonum), model kanonik 3D | yaw / pitch / roll (derajat) | — |
| Face embedding | **ArcFace w600k_r50** (`w600k_r50.onnx`) | vektor 512-dim, L2-normalized | 166,3 MB |
| Passive anti-spoof | **MiniFASNetV2** (`minifasnet_v2.onnx`) | 3 logit; kelas 1 = wajah hidup | 1,7 MB |

Model diambil dari dua paket InsightFace: **`buffalo_l.zip`** (275 MB → landmarker + embedder) dan **`buffalo_s.zip`** (122 MB → detektor). InsightFace tidak merilisnya sebagai `.onnx` satuan, jadi `modelctl` mengunduh paketnya lalu mengangkat anggota yang dibutuhkan.

**Detektornya SCRFD-500M, bukan SCRFD-10GF.** Terukur 60 sampel per konfigurasi, CPU 8 core — rincian dan metodologinya di [tasks/baseline.md](tasks/baseline.md):

| Model | 640 p50 | 640 p95 | 320 p50 | 320 p95 |
|---|---|---|---|---|
| **SCRFD-500M** | **131,9 ms** | 256,6 ms | 60,6 ms | 115,3 ms |
| SCRFD-10GF | 985,9 ms | 1269,5 ms | 305,8 ms | 477,2 ms |

Yang ringan di resolusi penuh mengalahkan yang berat di seperempat resolusi (132 ms vs 306 ms), jadi resolusi tidak perlu dikorbankan demi kecepatan.

⚠️ **Kriteria A4 (p95 < 150 ms) kemungkinan besar harus direvisi, bukan dikejar.** Detektor saja sudah memakai 256 ms p95 di 640, dan itu belum menghitung tiga model berikutnya. Ditinjau ulang di T13.

**MiniFASNetV2 tidak punya rilis ONNX resmi**, jadi ia dikonversi lokal dari checkpoint PyTorch upstream lewat container setup sekali jalan (`tools/antispoof/`). Container itu meng-clone repo upstream dan memakai definisi model serta bobot mereka sendiri — tidak ada arsitektur yang direproduksi dari ingatan. Service-nya sendiri tidak pernah butuh Python.

Pre-processing anti-spoof berbeda dari yang lain dan mudah salah: crop **2,7×** box wajah, resize ke 80×80 (anisotropik, sesuai training), nilai **[0,1]** tanpa pengurangan mean, dan urutan kanal **BGR** — bukan RGB seperti detektor.

Model **tidak** di-commit ke git dan **tidak** di-bake ke image. Diunduh sekali oleh `cmd/modelctl` ke volume `./models`, diverifikasi dengan SHA-256 yang tercatat di `models/manifest.json`.

---

## 3. Commands

Semua perintah dijalankan dari root project. **Toolchain Go tidak perlu terpasang di Windows** — build dan test berjalan di dalam container `dev` (menghindari kerumitan CGO + ONNX Runtime di Windows native).

```bash
# --- Setup awal (sekali saja) ---
cp .env.example .env
docker compose --profile setup run --rm modelctl download   # unduh + verifikasi checksum model

# --- Menjalankan ---
docker compose up -d --build          # api + postgres + minio
docker compose logs -f api
docker compose ps
docker compose down                   # stop, volume tetap
docker compose down -v                # stop + hapus SEMUA data (destruktif)

# --- Demo ---
# Buka http://localhost:8080/demo  (butuh izin kamera; gunakan localhost, bukan IP LAN)

# --- Test ---
docker compose run --rm dev go test ./... -race -count=1
docker compose run --rm dev go test ./... -coverprofile=coverage.out -covermode=atomic
docker compose run --rm dev go tool cover -func=coverage.out
docker compose run --rm dev go test -tags=integration ./tests/integration/... -timeout=10m

# --- Kualitas kode ---
docker compose run --rm dev golangci-lint run ./...
docker compose run --rm dev gofumpt -l -w .
docker compose run --rm dev go vet ./...

# --- Migrasi database ---
docker compose run --rm dev goose -dir migrations postgres "$DATABASE_URL" up
docker compose run --rm dev goose -dir migrations postgres "$DATABASE_URL" status
docker compose run --rm dev goose -dir migrations postgres "$DATABASE_URL" down

# --- Build ---
docker compose run --rm dev go build -trimpath -o bin/server ./cmd/server
docker build -f deploy/Dockerfile -t liveness-verifier:local .

# --- Utilitas ---
docker compose run --rm dev go run ./cmd/modelctl verify        # cek integritas model
docker compose run --rm dev go run ./cmd/bench -synthetic 60 -full   # latensi per tahap
docker compose run --rm dev go run ./cmd/bench -images <dir>    # sama, atas frame sungguhan
docker compose run --rm dev go run ./cmd/calibrate -h           # sapuan FAR/FRR
```

`-synthetic` membangkitkan adegannya sendiri, jadi benchmark bisa dijalankan
tanpa menaruh satu pun wajah sungguhan di repo. Ia mengukur **waktu**, bukan
kualitas deteksi; untuk yang kedua arahkan `-images` ke direktori di luar repo.

**Port yang dipakai:** `8080` API+demo · `5432` Postgres · `9000`/`9001` MinIO API/Console.

---

## 4. Project Structure

Pohon di bawah ini adalah bentuk yang **sudah berdiri**, bukan rencana.
[docs/STRUCTURE.md](docs/STRUCTURE.md) memuat versi lengkapnya beserta berkas
test; yang di sini dipangkas ke berkas yang membawa keputusan desain.

```
liveness-verifier/
├── SPEC.md                        # dokumen ini — source of truth
├── README.md                      # quickstart 5 menit
├── Jenkinsfile                    # CI: gerbang kualitas → 3 lapis test → image → deploy
├── compose.yaml                   # stack pengembangan
├── compose.ci.yaml                # overlay CI: `ports: !reset []`, tidak ada port terbit
├── go.mod / go.sum
├── .env.example                   # template konfigurasi (TIDAK berisi secret asli)
│
├── cmd/
│   ├── server/
│   │   ├── main.go                # entrypoint; juga -migrate dan -healthcheck
│   │   └── wire.go                # konstruksi dependency — satu-satunya tempatnya
│   ├── modelctl/                  # download + verifikasi checksum model ONNX
│   ├── bench/                     # benchmark latensi inference offline
│   └── calibrate/                 # sapuan FAR/FRR atas galeri berlabel
│
├── internal/
│   ├── config/                    # struct Config + parsing env + validasi saat boot
│   │
│   ├── httpapi/                   # LAPISAN TRANSPORT — tidak ada business logic
│   │   ├── router.go              # definisi rute chi
│   │   ├── middleware.go          # request ID, recover, timeout, API key, rate limit
│   │   ├── dto.go                 # struct request/response JSON + validasi
│   │   ├── errors.go              # mapping error domain → HTTP status + body
│   │   ├── ratelimit.go           # ember token per-kunci
│   │   ├── readiness.go           # /readyz: menyebut NAMA cek yang gagal
│   │   ├── web.go                 # menyajikan demo yang di-embed
│   │   ├── liveness_handler.go
│   │   └── faces_handler.go
│   │
│   ├── liveness/                  # DOMAIN — orkestrasi sesi challenge-response
│   │   ├── session.go             # entity Session, state machine, transisi
│   │   ├── challenge.go           # jenis challenge + generator urutan acak
│   │   ├── evaluator.go           # evaluasi 1 frame terhadap challenge aktif
│   │   ├── antireplay.go          # nonce, seq, dedup pHash, konsistensi identitas
│   │   └── service.go             # use case: Start / SubmitFrame / Complete
│   │
│   ├── enrollment/                # DOMAIN — galeri wajah, 1:N search
│   │   ├── service.go             # Enroll / Search / Get / Delete
│   │   ├── types.go               # Face, Repository, validasi
│   │   ├── token.go               # token sekali pakai, disimpan sebagai HMAC
│   │   ├── audit.go               # jejak yang tak terpisahkan dari barisnya
│   │   └── artifact.go            # retensi gambar — mati secara default
│   │
│   ├── calibrate/                 # sapuan FAR/FRR; menolak "target" yang hanya
│   │   └── sweep.go               #   tercapai dengan menolak semua orang jujur
│   │
│   ├── biometric/                 # PORT — interface, bebas dari ONNX
│   │   ├── types.go               # Face, BBox, Landmarks, Pose, Embedding
│   │   ├── ports.go               # Detector, Landmarker, AntiSpoofer, Embedder
│   │   ├── pipeline.go            # merangkai keempatnya jadi satu analisis frame
│   │   ├── pose.go                # perspektif-lemah: 106 landmark → yaw/pitch/roll
│   │   ├── metrics.go             # EAR (kedip), MAR (buka mulut), indeks landmark
│   │   ├── embedding.go           # kosinus, L2-normalisasi
│   │   ├── stub/                  # pipeline deterministik; default seluruh test
│   │   └── onnx/                  # ADAPTER — implementasi onnxruntime_go
│   │       ├── runtime.go         # init/teardown ORT env
│   │       ├── pool.go            # *ort.Session tidak aman untuk konkurensi
│   │       ├── detector_scrfd.go
│   │       ├── landmarker_2d106.go
│   │       ├── antispoof_minifas.go
│   │       └── embedder_arcface.go
│   │
│   ├── imaging/                   # decode, EXIF, resize, align 5-point, normalisasi
│   │   ├── decode.go              # batas byte DAN batas piksel — keduanya perlu
│   │   ├── exif.go
│   │   ├── align.go               # similarity transform ke template 112x112
│   │   ├── quality.go             # blur (Laplacian var), brightness, ukuran wajah
│   │   └── phash.go               # perceptual hash untuk deteksi frame duplikat
│   │
│   └── storage/
│       ├── postgres/              # ADAPTER — repository, pgvector codec
│       │   ├── db.go              # pool, VerifySchema, Migrate
│       │   ├── session_repo.go
│       │   ├── face_repo.go       # query HNSW 1:N; sisip wajah + audit satu transaksi
│       │   └── token_store.go     # belanja token secara atomik lewat UPDATE ... RETURNING
│       └── objectstore/           # ADAPTER — MinIO
│           └── minio.go
│
├── migrations/                    # goose SQL, penomoran urut
│   ├── 00001_init_extensions.sql
│   ├── 00002_liveness_sessions.sql
│   ├── 00003_session_retries.sql
│   ├── 00004_face_gallery.sql
│   ├── 00005_liveness_tokens.sql
│   ├── 00006_audit_log.sql
│   └── embed.go                   # //go:embed — migrasi ikut ke dalam biner
│
├── web/                           # di-embed via //go:embed
│   ├── embed.go
│   └── static/                    # demo: webcam → challenge → verdict
│       ├── index.html
│       ├── app.js
│       └── style.css
│
├── tools/
│   └── antispoof/                 # container sekali-jalan: PyTorch → ONNX
│       ├── Dockerfile             #   meng-clone repo hulu; tidak ada arsitektur
│       ├── convert.py             #   yang ditulis ulang dari ingatan
│       └── probe.py               # membuktikan ONNX == PyTorch, logit demi logit
│
├── docs/
│   ├── API.md                     # kontrak: alur, endpoint, kode error
│   ├── STRUCTURE.md               # pohon lengkap
│   └── DEPLOY.md                  # deploy pertama + cara membuktikan rollback
│
├── models/                        # .onnx + manifest.json  (DI-GITIGNORE, kecuali manifest)
│   └── manifest.json              # nama, URL, SHA-256, lisensi tiap model
│
├── tasks/
│   ├── plan.md                    # 31 task, 6 fase
│   ├── todo.md                    # status per task
│   └── baseline.md                # metodologi dan angka pengukuran detektor
│
├── tests/
│   ├── attack/                    # 10 serangan, TANPA build tag
│   └── integration/               # build tag `integration`, terhadap stack compose
│
└── deploy/
    ├── Dockerfile                 # multi-stage: builder (CGO) → runtime slim
    └── deploy.sh                  # rilis + gerbang kesehatan + rollback
```

**Aturan dependensi (ditegakkan lewat review, bukan hanya konvensi):**

```
httpapi ──→ liveness / enrollment ──→ biometric (port) ──→ biometric/onnx (adapter)
                     │                      ▲
                     └──→ storage/*  ───────┘   imaging
```

- `internal/biometric` **tidak boleh** mengimpor `onnxruntime_go`. Hanya `internal/biometric/onnx` yang boleh.
- `internal/liveness` dan `internal/enrollment` **tidak boleh** mengimpor `net/http` atau `chi`.
- Wiring semua dependency terjadi **hanya** di `cmd/server/wire.go`.

---

## 5. API Surface

Semua endpoint di-prefix `/v1`. Body JSON, gambar dikirim sebagai base64 di
field `frame` (batas 2 MB per frame, **dan** 16 juta piksel setelah decode —
batas byte saja tidak cukup, sebuah PNG 200 kB bisa mekar jadi gigabyte).

**Tidak semua endpoint memakai `X-API-Key`, dan itu disengaja.** Kolom Auth di
bawah adalah bagian dari kontrak:

### Liveness

| Method | Path | Auth | Fungsi |
|---|---|---|---|
| `POST` | `/v1/liveness/sessions` | `X-API-Key` | Buat sesi. Response: `session_id`, `nonce`, daftar `challenges` (urutan diacak), `expires_at`. |
| `POST` | `/v1/liveness/sessions/{id}/frames` | **nonce sesi** | Kirim 1 frame + `seq` + `client_ts`. Response: challenge aktif, progres, `advanced` (bool), alasan jika ditolak. |
| `POST` | `/v1/liveness/sessions/{id}/complete` | **nonce sesi** | Finalisasi. Response: `state`, `passed`, dan `token` bila lolos. |
| `GET` | `/v1/liveness/sessions/{id}` | **nonce sesi** | Status sesi (tanpa data biometrik mentah). |

Tiga endpoint bernonce itu berjalan di peramban subjek. API key adalah kredensial
**operator**; menaruhnya di peramban orang yang sedang diverifikasi adalah cara
kredensial itu berakhir di tempat yang tidak seharusnya. Yang menggantikannya:
`session_id` dan `nonce`, masing-masing 128 bit acak, kedaluwarsa bersama
sesinya, dan dicek dalam waktu konstan. Memegang keduanya adalah kapabilitasnya.

Membuat sesi tetap butuh kunci karena ia mengalokasikan baris database dan slot
kerja inference — itu harus bisa diatribusikan.
`LV_ALLOW_ANONYMOUS_SESSIONS=true` membuka **hanya rute itu**, dan server
memperingatkannya di tiap boot.

### Faces

| Method | Path | Auth | Fungsi |
|---|---|---|---|
| `POST` | `/v1/faces` | `X-API-Key` **+** `token` | Enroll. Token liveness membuktikan penangkapannya hidup; kunci membuktikan siapa yang boleh menulis ke galeri. Tidak satu pun menggantikan yang lain. |
| `POST` | `/v1/faces/search` | `X-API-Key` | **1:N** — satu gambar → top-K kandidat + `score`, `matched` (bool). |
| `DELETE` | `/v1/faces` | `X-API-Key` | Hapus subject: embedding + baris audit. Hard delete. `subject_id` di **body**. |

**`/v1/faces/verify` dan `GET /v1/faces/{subject_id}` tidak dibangun.** Spec ini
sendiri menandai yang pertama opsional; yang kedua tidak dipakai jalur mana pun.
Menambahkannya nanti tidak merusak apa pun yang sudah ada.

**`subject_id` ada di body, bukan di path — ini penyimpangan yang disengaja
dari REST konvensional.** Segmen path terbaca sebagai pilihan yang benar, dan di
sini salah: `subject_id` ditentukan integrator dan lazimnya nomor identitas atau
nomor rekening, sementara path mendarat di log akses, log proxy, dan riwayat
peramban. Penalaran yang sama sudah menjaganya keluar dari query string.

**`token` bukan JWT.** Spec versi 0.1 menyebutnya JWT; implementasinya token
acak 256 bit yang disimpan sebagai HMAC. Tanda tangan bisa membuktikan token
diterbitkan di sini dan belum kedaluwarsa, tapi **tidak bisa membuktikan belum
pernah dipakai** — dan "dipakai tepat sekali" justru inti pertahanannya. Sekali
pakai menuntut state; begitu state ada, tanda tangan tidak menambah apa pun.

### Operasional

`GET /healthz` (prosesnya hidup) · `GET /readyz` (database, versi skema, object
store — **bukan** "model ter-load"; model dimuat saat boot dan gagal di sana,
jadi proses yang modelnya tidak ada tidak pernah sampai menerima probe) ·
`GET /demo` (UI). Ketiganya tanpa kunci.

`/readyz` menyebut **nama** cek yang gagal di response-nya. Itu bocoran kecil
yang sengaja diterima: probe ini tidak terekspos ke publik, dan deploy yang
gagal pada pukul tiga pagi butuh tahu *mana* yang mati tanpa harus membuka log.

### Anti-replay: lapisan pertahanan

Sesi dianggap valid hanya jika **semua** ini terpenuhi:

1. **Challenge acak** — 3 challenge dipilih dan diurutkan acak per sesi; video rekaman tidak akan cocok dengan urutan yang belum diketahui.
2. **Nonce + sequence** — `seq` harus naik monoton; frame duplikat/ mundur ditolak.
3. **Batas waktu** — sesi kedaluwarsa dalam 90 detik; tiap challenge maksimal 5 detik, dihitung mundur di layar. Challenge yang kehabisan waktu diulang, bukan menggagalkan sesi — maksimal 2 kali untuk seluruh sesi (`LV_LIVENESS_MAX_RETRIES`), dengan progres challenge direset supaya sebuah kedipan tidak bisa dirakit dari dua percobaan. TTL sesi adalah batas keras yang tidak bisa dilampaui berapa pun sisa kesempatannya.
4. **Dedup perceptual hash** — dua frame dengan Hamming distance pHash < 5 dianggap frame yang sama (indikasi replay statis).
5. **Passive anti-spoof per frame** — MiniFASNetV2; frame di bawah ambang menggagalkan sesi. **⚠ Saat ini TIDAK ditegakkan** (`LV_LIVENESS_ANTISPOOF_ENFORCE=false`). Konversi yang dipakai memberi skor ~0,006 untuk wajah asli terhadap ambang 0,80 — terukur pada sesi sungguhan, dan jalur Go sudah dibuktikan menghasilkan logits identik dengan PyTorch aslinya, jadi ini bukan bug implementasi. Tersangka tersisa: konvensi kotak SCRFD berbeda dari detektor upstream sehingga crop 2,7× jatuh di luar distribusi latih, dan pipeline ini memakai separuh ensemble (upstream menjumlahkan dua model). Keduanya tidak bisa dipastikan tanpa Open Question #3. **Selama ini false, sistem tidak memblokir foto cetak maupun replay layar, dan tidak boleh disebut PAD-compliant.** Skor tetap diukur, dicatat, dan diperingatkan tiap boot.
6. **Konsistensi identitas lintas frame** — embedding ArcFace tiap frame kunci harus cosine ≥ 0.70 terhadap frame pertama. Mencegah pergantian wajah di tengah sesi.

### Threshold awal (bukan hasil kalibrasi — lihat §9)

| Parameter | Nilai berlaku | Sumber |
|---|---|---|
| Skor deteksi wajah minimum | 0.60 | default SCRFD |
| Lebar wajah minimum | 112 px | ukuran input ArcFace — **bukan** angka yang boleh diturunkan |
| Kedip | rasio **0.60 / 0.85** terhadap bukaan terlebar subjek | **diukur**, menggantikan EAR mutlak |
| Yaw untuk tengok kiri/kanan | pergeseran ≥ **15°** dari baseline | **diukur**, turun dari 25° |
| Pitch untuk angguk | pergeseran ≥ 15° dari baseline | ditetapkan |
| MAR buka mulut | ≥ 0.55 | satu-satunya ambang di blok ini yang **terbukti bekerja** |
| Skor passive liveness minimum | 0.80 — **tidak ditegakkan** | lihat lapisan 5 di atas |
| Cosine match 1:N | ≥ 0.42 | rekomendasi umum buffalo_l |
| Konsistensi identitas | cosine ≥ 0.70 | dipakai juga untuk mengikat wajah enrollment |
| Blur minimum (Laplacian variance) | > 80 | ditetapkan, perlu kalibrasi |

**Ambang kedip diubah dari mutlak ke relatif, dan itu perbaikan bug, bukan
penyetelan.** Angka 0.21/0.30 adalah literatur untuk skema landmark dlib 68
titik; model ini 106 titik dengan pemilihan indeks sendiri. Pada sesi sungguhan
pipeline ini menghasilkan EAR 0,030–0,300 dengan rata-rata 0,124 — **nol dari 34
frame** pernah mencapai ambang "terbuka" 0,30. Challenge kedip bukan sulit
dipenuhi, melainkan mustahil. Sekarang diukur sebagai penurunan terhadap bukaan
terlebar subjek itu sendiri, penalaran yang sama yang sejak awal dipakai untuk
gerakan menoleh.

**Yaw turun ke 15° karena subjek yang jelas-jelas menoleh hanya menghasilkan 22°.**
Kecurigaan yang belum terbukti dan sengaja dicatat: pitch terbaca 40–72° padahal
subjek menghadap layar laptop, yang tidak masuk akal secara fisik. Kalau benar
ada bias sebesar itu, akar masalahnya di estimasi pose dan bukan di ambang ini.

⚠️ **Ini bukan kalibrasi.** Angka yang ditandai "diukur" berasal dari **satu
orang, satu kamera, satu ruangan, 34 frame** — cukup untuk membuktikan angka
lama salah, sama sekali tidak cukup untuk menyebut yang baru benar. Open
Question #2 dan #3 tetap terbuka, dan `cmd/calibrate` sudah siap dijalankan
begitu datanya ada.

Semua nilai di atas **wajib** dapat dioverride lewat env var. Tidak boleh ada angka ajaib yang di-hardcode di tengah logika.

---

## 6. Code Style

Ikuti *Effective Go* + *Google Go Style Guide*. Format dengan `gofumpt`. Contoh nyata yang menetapkan standar:

```go
// Package liveness orchestrates challenge-response verification sessions.
package liveness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// Sentinel errors are declared at package level so callers can use errors.Is.
var (
	ErrSessionExpired  = errors.New("liveness: session expired")
	ErrSequenceReplay  = errors.New("liveness: sequence number went backwards or repeated")
	ErrSpoofDetected   = errors.New("liveness: frame flagged as spoof")
	ErrIdentityChanged = errors.New("liveness: identity changed mid-session")
)

// FrameResult is the outcome of evaluating a single frame against the active
// challenge.
type FrameResult struct {
	Challenge     ChallengeKind `json:"challenge"`
	Advanced      bool          `json:"advanced"`
	CompletedAll  bool          `json:"completed_all"`
	LivenessScore float64       `json:"liveness_score"`
	Reason        string        `json:"reason,omitempty"`
}

// SubmitFrame evaluates one frame against the currently active challenge.
//
// Frames rejected for low quality (blurry, face too small, no face found) do
// NOT fail the session — the caller is expected to send another frame. Only a
// spoof signal or an identity change fails the session outright.
func (s *Service) SubmitFrame(ctx context.Context, sessionID SessionID, f Frame) (FrameResult, error) {
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return FrameResult{}, fmt.Errorf("get session %s: %w", sessionID, err)
	}

	if s.clock.Now().After(sess.ExpiresAt) {
		return FrameResult{}, ErrSessionExpired
	}
	if f.Seq <= sess.LastSeq {
		return FrameResult{}, ErrSequenceReplay
	}

	face, err := s.pipeline.Analyze(ctx, f.Image)
	if err != nil {
		// Low quality is recoverable: report it and let the client retry.
		if errors.Is(err, biometric.ErrNoFaceFound) || errors.Is(err, biometric.ErrLowQuality) {
			return FrameResult{Challenge: sess.ActiveChallenge(), Reason: err.Error()}, nil
		}
		return FrameResult{}, fmt.Errorf("analyze frame seq=%d: %w", f.Seq, err)
	}

	if face.SpoofScore < s.cfg.MinLivenessScore {
		s.log.WarnContext(ctx, "session failed: spoof detected",
			slog.String("session_id", sessionID.String()),
			slog.Float64("spoof_score", face.SpoofScore),
		) // NOTE: never log raw images or embeddings.
		return FrameResult{}, ErrSpoofDetected
	}

	return s.advance(ctx, sess, face)
}
```

> **Satu hal yang sketsa di atas salah, dan biayanya mahal.**
> Ia memeriksa penjaga lalu `return` sebelum evaluator melihat frame-nya. Kode
> sungguhannya sempat begitu, dan akibatnya: subjek yang menahan pose — persis
> yang diminta challenge — menghasilkan frame yang mirip frame sebelumnya,
> penjaga duplikat menolaknya, dan gerakan yang **sudah benar** tidak pernah
> sampai ke evaluator. Sesinya habis waktu sementara subjek melakukan hal yang
> tepat. Bentuk yang benar memisahkan penolakan yang **fatal** dari yang
> **bisa dipulihkan**, dan hanya yang pertama boleh menghentikan evaluasi:
>
> ```go
> // A recoverable rejection must not stop the frame being evaluated.
> if guardErr := s.deps.Guard.CheckAnalysis(session, frame, face); Fatal(guardErr) {
>     return s.endSession(ctx, session, now, failSession, guardErr.Error())
> }
> outcome := s.deps.Evaluator.Evaluate(session, face)
> ```
>
> Dibiarkan di sini beserta perbaikannya, bukan diam-diam ditukar: contoh yang
> menetapkan standar sebaiknya juga menunjukkan apa yang pernah menjatuhkannya.

**Konvensi yang mengikat:**

| Aturan | Detail |
|---|---|
| Error | Selalu bungkus dengan `fmt.Errorf("konteks: %w", err)`. Sentinel error untuk kondisi yang diperiksa pemanggil. **Tidak ada `panic`** di luar `main()`. |
| Context | `ctx context.Context` selalu parameter pertama. Diteruskan sampai ke query DB dan inference. |
| Interface | Dideklarasikan di sisi **konsumen** (`internal/biometric/ports.go`), bukan di sisi implementasi. Ukuran kecil (1–3 method). |
| Konstruktor | `New*` mengembalikan `(*T, error)`; validasi semua dependency wajib di sini, bukan saat dipakai. |
| Waktu | Lewat interface `Clock` yang bisa di-fake. Tidak ada `time.Now()` langsung di dalam domain. |
| Konkurensi | Satu `*ort.Session` per model tidak thread-safe → dibungkus pool `chan *Session`. Semua state bersama dilindungi mutex atau channel; `-race` harus bersih. |
| Nama | Paket: satu kata, huruf kecil (`liveness`, bukan `livenessService`). Hindari stutter: `liveness.Service`, bukan `liveness.LivenessService`. |
| Logging | `slog` terstruktur. Field wajib: `session_id`, `request_id`. **Dilarang** mencatat gambar, embedding, atau base64 payload. |
| Komentar | Menjelaskan **mengapa**, bukan **apa**. Semua identifier yang diekspor punya doc comment berawalan namanya. |
| **Bahasa** | **Semua isi kode berbahasa Inggris**: identifier, komentar, doc comment, pesan error, pesan log, nama test, nama kolom & migrasi SQL, dan pesan error yang dikembalikan API. **Dokumentasi berbahasa Indonesia**: `SPEC.md`, `README.md`, `tasks/*.md`, commit message, dan teks di demo UI. Tidak ada campuran di dalam satu file kode. |
| SQL | Query eksplisit di file repository. Tidak ada ORM. Selalu parameterized — tidak ada string concatenation. |

---

## 7. Testing Strategy

| Level | Lokasi | Cakupan | Perintah |
|---|---|---|---|
| **Unit** | `*_test.go` bersebelahan dengan source | State machine sesi, EAR/MAR/pose math, anti-replay, parsing config, threshold. Semua dependency di-fake. | `go test ./... -race` |
| **Golden** | `internal/biometric/onnx/testdata/golden/` (tag `models`) | Output detector/landmarker/embedder terhadap adegan yang dibangkitkan secara deterministik. Mendeteksi regresi diam-diam saat ganti versi model. | `go test -tags=models ./internal/biometric/onnx/...` |
| **Integrasi** | `tests/integration/` (tag `integration`) | Postgres dan MinIO asli dari stack compose. Migrasi naik-turun-naik, atomisitas audit, token sekali pakai. Kriteria B diukur di sini, bukan di benchmark: 10.000 embedding, p95 pencarian, dan recall@1 HNSW dibandingkan brute force. | `go test -tags=integration ./tests/integration/...` |
| **Serangan** | `tests/attack/` — **tanpa build tag** | Sepuluh serangan lewat router HTTP sungguhan dengan pipeline stub: replay urutan, urutan mundur, gambar diam ditahan, nonce salah pada tiga endpoint, sesi diselesaikan lebih awal, sesi kedaluwarsa, tanpa API key, urutan challenge yang bisa ditebak, dan penolakan yang membocorkan pertahanan mana yang bekerja. Plus satu yang membuktikan subjek jujur yang diam sebentar **tidak** ditolak. | `go test ./tests/attack/...` |
| **Benchmark** | `cmd/bench/` (perintah, bukan `go test -bench`) | Latensi per tahap inference terhadap model asli. Ditulis sebagai perintah karena ia butuh berkas `.onnx` dan gambar sungguhan — dua hal yang menurut aturan 1 di bawah tidak boleh dituntut oleh `go test ./...`. | `go run ./cmd/bench -synthetic 60 -full` |

**Target coverage:**

- `internal/liveness`, `internal/enrollment`, `internal/biometric` (kecuali `onnx/`): **≥ 80%**
- Keseluruhan repo: **≥ 70%**
- `internal/biometric/onnx/`: dikecualikan dari target coverage (butuh model asli); dijaga oleh golden test.

> Per 0.3 **kedua target pertama belum terpenuhi**: `internal/enrollment` di
> 78,0% dan repo di 55,8%. Angkanya tidak diturunkan agar cocok dengan hasil —
> itu akan menghapus satu-satunya alasan angka itu ada. Rincian dan pertanyaan
> yang menyertainya ("keseluruhan repo" itu penyebutnya apa?) di §9 X2.

**Aturan test:**

1. Semua test **harus** bisa jalan tanpa file model `.onnx` kecuali yang bertag `models`. Stub pipeline adalah default.
2. Test **tidak boleh** menyentuh jaringan luar. Model dan container di-pin dan lokal.
3. **Tidak ada foto wajah orang asli** di `testdata/` mana pun. Ini bukan preferensi — ini batas hukum data biometrik. Sampai 0.3 aturan ini dipatuhi dengan cara yang lebih kuat dari yang diminta: repo tidak memuat satu pun gambar wajah, sungguhan maupun sintetis. Adegan uji dibangkitkan oleh kode saat test berjalan, dan golden file menyimpan **angkanya**, bukan gambarnya.
4. Bug fix mengikuti pola *Prove-It*: tulis test yang gagal dan mereproduksi bug **dulu**, baru perbaiki.
5. `go test -race` harus bersih. Data race pada pool ONNX session adalah kegagalan build.

---

## 8. Boundaries

### ✅ Always (lakukan tanpa perlu bertanya)

- Jalankan `go test ./... -race` dan `golangci-lint run` sebelum menyatakan sebuah task selesai.
- Validasi semua input di batas `httpapi`: ukuran gambar, tipe MIME, dimensi, panjang field, rentang `seq`.
- Bungkus setiap error dengan konteks; jangan pernah menelan error diam-diam.
- Ekspresikan setiap threshold/timeout sebagai konfigurasi env, dengan default eksplisit di `internal/config`.
- Tulis migrasi `goose` sebagai pasangan `Up` **dan** `Down` yang benar-benar berfungsi.
- Perbarui `SPEC.md` ini terlebih dahulu ketika sebuah keputusan desain berubah, baru ubah kode.
- Perlakukan gambar wajah dan embedding sebagai data pribadi sensitif: enkripsi saat transit, akses terbatas, tidak pernah masuk log.
- Commit dalam potongan kecil yang berdiri sendiri, dengan test menyertai perubahannya.

### ⚠️ Ask first (hentikan dan konfirmasi dulu)

- Menambah dependency Go baru di luar yang tercantum di §2.
- Mengubah skema database setelah migrasi pertama di-commit.
- Mengubah bentuk request/response API yang sudah ada (breaking change kontrak).
- Mengganti model ONNX atau versinya — mengubah distribusi skor sehingga semua threshold perlu dikalibrasi ulang.
- Menambah container ke `compose.yaml`.
- Menurunkan threshold keamanan mana pun agar sebuah test lolos.
- Menambah eksekusi ONNX di GPU (CUDA/DirectML) — mengubah base image dan matriks build secara signifikan.
- Melewatkan atau menonaktifkan verifikasi liveness pada jalur enrollment.

### 🚫 Never (jangan pernah, dalam kondisi apa pun)

- Commit file `.onnx`, `.env` asli, kunci API, kredensial database, atau foto wajah orang sungguhan.
- Mencatat (log) gambar mentah, base64 frame, atau vektor embedding — dalam level log apa pun, termasuk debug.
- Kirim data biometrik ke layanan eksternal mana pun. Sistem ini offline; tidak ada egress selain unduhan model saat setup.
- Menghapus atau men-skip test yang gagal untuk membuat build hijau.
- Menaruh gambar, `subject_id`, atau data pribadi di query string URL.
- Menyimpan embedding tanpa baris audit yang menyertainya.
- Menerapkan `SELECT *` pada tabel yang memuat kolom embedding di jalur yang mengembalikan response ke klien.
- Mengklaim sistem ini "PAD-compliant", "tersertifikasi", atau siap produksi eKYC. Tidak, dan spec ini tidak menargetkan itu.
- Menonaktifkan verifikasi TLS atau memakai kredensial default MinIO/Postgres di konfigurasi mana pun selain `.env.example`.

---

## 9. Success Criteria

Selesai berarti **semua** poin berikut dapat diverifikasi:

### Milestone A — Active Liveness

| # | Kriteria | Cara verifikasi |
|---|---|---|
| A1 | `docker compose up -d --build` menghidupkan seluruh service dalam keadaan healthy, < 5 menit dari cache kosong. | `docker compose ps` menunjukkan semua `healthy`. |
| A2 | `GET /readyz` mengembalikan 200; **503** bila database, skema, atau object store tidak siap. Model dimuat saat boot dan gagal di sana, bukan di readiness — proses yang modelnya tidak ada tidak pernah sampai menerima probe. | ✅ **lolos** — diverifikasi dengan mematikan Postgres: `/readyz` 503 sementara `/healthz` tetap 200 |
| A3 | Demo di `http://localhost:8080/demo` menuntaskan sesi 3-challenge memakai webcam, verdict muncul < 2 detik setelah frame terakhir. | ⚠ **sebagian.** Demo berjalan dengan webcam sungguhan dan menemukan empat cacat yang lolos dari seluruh test yang ada: frame diturunkan ke 480 px sehingga wajah 105 px ditolak ambang 112 px; ambang kedip mutlak yang tidak pernah dicapai siapa pun; anti-spoof menolak 100% subjek asli; dan model pose yang nyaris sebidang. Keempatnya diperbaiki. Yang **belum** diukur adalah bagian "< 2 detik" — tidak ada instrumentasi yang mencatatnya di klien. |
| A4a | Frame biasa (tanpa embedder) **p95 < 150 ms**. Terukur **149,1 ms** di input 320. | `bench -full -size 320` |
| A4b | Frame kunci (dengan embedder) **p95 < 900 ms**. Terukur **796,4 ms**. Anggaran terpisah karena frame kunci muncul beberapa kali per sesi, bukan enam kali per detik. | `bench -full -size 320` |
| A5 | Print attack (foto dicetak / ditampilkan di layar HP) ditolak. | 🚫 **TIDAK TERPENUHI, dan tidak akan terpenuhi tanpa kalibrasi.** Konversi MiniFASNetV2 yang ada memberi skor ~0,006 pada wajah sungguhan — dengan penegakan menyala ia menolak **setiap** subjek asli, jadi `LV_LIVENESS_ANTISPOOF_ENFORCE` default `false` dan foto cetak lolos. Skornya tetap diukur dan dicatat. Konversinya terbukti setia pada PyTorch (`tools/antispoof/probe.py`, logit demi logit), jadi yang kurang adalah kalibrasi, bukan kode. Terhubung ke Open Question #2 dan #3. |
| A6 | Replay attack (video rekaman orang yang sama) gagal karena urutan challenge acak. | ✅ `TestChallengeOrderIsNotFixedAcrossSessions` + `TestAStillImageHeldTooLongEndsTheSession` |
| A7 | Frame duplikat, `seq` mundur, dan pergantian identitas di tengah sesi ditolak dengan kode error yang tepat. | ✅ `tests/attack/` — dan `TestHoldingStillBrieflyIsNotAnAttack` membuktikan pertahanannya tidak menelan subjek jujur |
| A8 | Sesi kedaluwarsa otomatis pada 90 detik; sesi kedaluwarsa dibersihkan dari DB. | ✅ `TestAnExpiredSessionCannotBeUsed` + uji integrasi dengan jam palsu |

### Milestone B — Enrollment & 1:N

| # | Kriteria | Cara verifikasi |
|---|---|---|
| B1 | Enrollment **menolak** request tanpa `liveness_token` yang valid dan belum terpakai. | ✅ `TestEnrollNeedsAValidToken`, `TestTokenIsRedeemedOnceAndOnlyOnce`, dan `TestAFailedEnrollmentStillSpendsTheToken` — yang terakhir menutup celah menggilir token yang sama sampai satu percobaan berhasil |
| B2 | 1:N search pada galeri 10.000 embedding: **p95 < 50 ms** dengan index HNSW. | ✅ **lolos dengan margin** — terukur **6,4 ms** p95 di `tests/integration/face_gallery_test.go` |
| B3 | Recall@1 index HNSW ≥ 0.98 dibanding brute force exact search pada dataset yang sama. | ✅ terukur **1,000** pada galeri dan probe yang sama |
| B4 | `DELETE /v1/faces` menghapus embedding **dan** objek MinIO, serta menyisakan baris audit. | ✅ uji integrasi. `subject_id` di body, bukan di path — lihat tabel ratifikasi §11 |
| B5 | Setiap keputusan verifikasi menghasilkan baris audit yang dapat ditelusuri lengkap dengan skor dan referensi artefak. | ✅ ditegakkan oleh tipe, bukan oleh disiplin: `Repository.Insert(ctx, Face, AuditEntry)` menerima keduanya dalam satu transaksi, jadi menyimpan embedding tanpa audit bukan sesuatu yang bisa dipanggil |

### Lintas milestone

| # | Kriteria | Cara verifikasi |
|---|---|---|
| X1 | `go test ./... -race` lolos bersih. | ✅ **lolos** — 2026-08-10, seluruh paket, nol data race |
| X2 | Coverage domain ≥ 80%, repo ≥ 70%. | ⚠ **BELUM TERPENUHI pada dua hitungan.** Lihat kotak di bawah tabel. |
| X3 | `golangci-lint run ./...` nol issue. | ✅ **lolos** — 2026-08-10, nol keluaran |
| X4 | Test khusus membuktikan tidak ada gambar/embedding yang bocor ke log. | ✅ `internal/httpapi/logleak_test.go` — menangkap keluaran `slog` dan memindainya |
| X5 | Migrasi `up` lalu `down` lalu `up` kembali berhasil pada database kosong. | ✅ `tests/integration/postgres_test.go` |
| X6 | README memungkinkan orang lain menjalankan project ini dari nol dalam < 10 menit. | ⚠ **belum diuji ulang di mesin bersih.** README ada dan lengkap, tapi klaimnya belum pernah dibuktikan oleh siapa pun selain penulisnya. |

> #### X2 — angka sebenarnya, per 2026-08-10
>
> | Ukuran | Target | Terukur | |
> |---|---|---|---|
> | `internal/enrollment` | ≥ 80% | **78,0%** | ⚠ kurang dua poin |
> | `internal/liveness` | ≥ 80% | 89,8% | ✅ |
> | `internal/biometric` | ≥ 80% | 94,3% | ✅ |
> | Keseluruhan repo | ≥ 70% | **55,8%** | ⚠ kurang empat belas poin |
>
> Kekurangan kedua lebih besar dari yang terlihat, dan penyebabnya bukan kode
> yang tidak diuji. `internal/storage/*` dan `cmd/*` menyumbang nol ke profil
> ini: adapter storage diuji oleh `tests/integration/` yang butuh Postgres dan
> MinIO hidup dan tidak ikut terhitung, sementara `cmd/*` adalah entrypoint yang
> memang tidak punya test. Keluarkan keduanya dan angkanya jadi **88,0%**;
> keluarkan `cmd/` dan `biometric/onnx/` saja — dua yang sudah §7 kecualikan —
> dan jadi **74,8%**, di atas target.
>
> Itu **bukan** izin untuk menulis 74,8% lalu menyatakan lolos. Yang sebenarnya
> terjadi adalah §7 menetapkan "keseluruhan repo" tanpa pernah menyebut apa yang
> ada di dalam kata itu, dan penyebut yang tidak pernah didefinisikan bisa
> digeser sampai kriteria mana pun lolos. Angka literalnya 55,8%. Memilih
> penyebut yang benar adalah keputusan Anda, bukan sesuatu yang boleh dipungut
> di sini sambil lalu.

---

## 10. Open Questions

Perlu jawaban Anda — beberapa memblokir kalibrasi, sisanya bisa diputuskan sambil jalan:

1. **Lisensi model.** Model InsightFace pre-trained berlisensi riset non-komersial. Apakah project ini akan tetap di ranah riset/personal, atau perlu jalur model berlisensi komersial (mis. melatih ulang, atau memakai model berlisensi permisif dengan akurasi lebih rendah)? — *Memblokir keputusan model.*

2. **Target FAR/FRR.** Berapa target False Accept Rate dan False Reject Rate? Tanpa angka, threshold di §5 hanyalah tebakan berdasar literatur. Standar industri eKYC: FAR ≤ 0.01%, FRR ≤ 5%. — *Memblokir kalibrasi threshold.*

3. **Dataset kalibrasi.** Akan pakai apa untuk mengukur? CelebA-Spoof / CASIA-SURF / rekaman sendiri? — *Memblokir A5, yang saat ini **gagal**.* Tanpa data berlabel, ambang anti-spoof tidak bisa dipindahkan dari 0,80 dengan alasan apa pun selain tebakan, dan `cmd/calibrate` — yang sudah dibangun dan diuji — tidak punya apa-apa untuk disapu.

4. **Retensi data.** Berapa lama artefak frame dan baris audit disimpan? Perlukah job pembersihan otomatis? UU PDP Indonesia mensyaratkan batas retensi eksplisit untuk data biometrik.

5. **Skala galeri.** Berapa perkiraan jumlah subject terdaftar? 10k dan 10 juta membutuhkan strategi index yang berbeda secara fundamental (HNSW in-memory vs sharding).

6. ~~**Streaming frame.**~~ ✅ **TERJAWAB dengan dibangun (2026-08-08):** HTTP POST per frame, ~6 fps. Anggaran terukur 149 ms per frame membuat laju itu pas; lebih cepat hanya menumpuk antrean. WebSocket tetap di backlog dan tidak mengubah kontrak mana pun kalau ditambahkan nanti.

7. **Otentikasi.** Static API key sudah cukup untuk pemakaian lokal? Atau perlu langsung OIDC/mTLS?

8. ~~**Bahasa dokumentasi & kode.**~~ ✅ **TERJAWAB (2026-08-07):** kode berbahasa Inggris, dokumentasi berbahasa Indonesia. Lihat baris "Bahasa" di §6.

9. ~~**Model anti-spoof pasif.**~~ ✅ **TERJAWAB (2026-08-07):** opsi (a) — konversi `.pth` → ONNX lewat container setup sekali jalan di `tools/antispoof/`, hasilnya di-pin di manifest seperti artefak lain. Container-nya memakai definisi model dan bobot upstream sendiri, jadi tidak ada arsitektur yang direproduksi dari ingatan. Runtime service tetap murni Go.

---

## 11. Riwayat Revisi

| Versi | Tanggal | Perubahan |
|---|---|---|
| 0.1 | 2026-08-06 | Draft awal. Scope: active liveness + enrollment/1:N, Go + ONNX Runtime, REST + demo UI, Postgres/pgvector + MinIO. |
| 0.2 | 2026-08-07 | **Spec di-approve.** Open question #8 ditutup: kode Inggris, dokumen Indonesia. Contoh di §6 ditulis ulang. Lanjut ke Phase 2 (Plan). |
| 0.3 | 2026-08-10 | **Spec dikejar agar menyusul implementasi.** Dua belas penyimpangan diratifikasi, semuanya keputusan yang sudah diambil di kode dengan alasannya di commit tapi tidak pernah dibawa kembali ke sini — §8 mensyaratkan urutan sebaliknya, dan itu berulang kali dilanggar. Tiga kriteria §9 dicatat **gagal** tanpa targetnya diturunkan. Rinciannya di bawah. |

### Yang diratifikasi di 0.3, dan mengapa

| Spec 0.1–0.2 menjanjikan | Yang dibangun | Alasan |
|---|---|---|
| `liveness_token` berupa **JWT** | token acak 256 bit, disimpan sebagai HMAC | Tanda tangan tidak bisa membuktikan token belum dipakai; sekali-pakai menuntut state, dan begitu state ada tanda tangan tidak menambah apa pun |
| `DELETE /v1/faces/{subject_id}` | `subject_id` di **body** | Path mendarat di log akses, log proxy, dan riwayat peramban — dan `subject_id` lazimnya nomor identitas |
| EAR kedip mutlak `< 0.21` | rasio terhadap bukaan mata subjek sendiri | Nol dari 34 frame pernah mencapai ambang lama; challenge-nya mustahil, bukan sulit |
| Yaw `> 25°` | **15°** | Subjek yang jelas menoleh hanya menghasilkan 22° |
| Challenge kehabisan waktu → sesi gagal | **diulang**, maksimal 2× per sesi | Telat satu langkah membuang setiap langkah yang sudah lolos |
| `/readyz` melaporkan "4 model ter-load" | database, versi skema, object store | Model dimuat saat boot; proses yang gagal memuatnya tidak pernah sampai menerima probe |
| `testify` + `testcontainers-go` | `testing` stdlib, stack compose | Satu dependensi lebih sedikit di jalur data biometrik; testcontainers menuntut socket Docker yang tidak dibutuhkan |
| `/v1/faces/verify`, `GET /v1/faces/{id}` | **tidak dibangun** | Spec sendiri menandai yang pertama opsional; yang kedua tidak dipakai jalur mana pun |
| 4 migrasi | **6** | `retries`, `liveness_tokens`, `face_audit` |
| PnP penuh untuk head pose | **perspektif-lemah** (ortografik berskala) | Solusi penuh menuntut focal length; webcam tidak melaporkannya, dan menebaknya memasukkan galat yang lebih besar dari yang dihilangkan |
| Benchmark sebagai `*_bench_test.go` | perintah `cmd/bench` | Benchmark butuh berkas `.onnx` dan gambar sungguhan — dua hal yang aturan 1 di §7 melarang `go test ./...` menuntutnya |
| `testdata/` berisi wajah sintetis | **tidak ada gambar wajah sama sekali** | Adegan dibangkitkan kode saat test jalan; golden file menyimpan angka, bukan gambar. Lebih ketat dari yang diminta, jadi dibiarkan |

Ditambah dua hal yang tidak ada di spec mana pun dan sekarang ada: perintah
`-migrate` pada server (`postgres.Migrate` sudah ada sejak T14 tapi **tidak
pernah dipanggil siapa pun**), dan `internal/calibrate` beserta `cmd/calibrate`
— harness ambang yang siap dijalankan begitu Open Question #2 dan #3 terjawab.

### Yang TIDAK diratifikasi — dan sengaja dibiarkan merah

Tiga kriteria di §9 gagal, dan tidak satu pun diperbaiki dengan cara menurunkan
targetnya. Menuliskannya di sini supaya tidak hilang di antara yang hijau:

- **A5 — print attack tidak ditolak.** Anti-spoof pasif mati secara default
  karena dengan menyala ia menolak setiap subjek asli. Server memperingatkan di
  tiap boot. Butuh kalibrasi (Open Question #2, #3), bukan kode.
- **X2 — coverage.** `internal/enrollment` 78,0%; repo 55,8%. Yang kedua
  membuka pertanyaan yang §7 tidak pernah jawab: "keseluruhan repo" itu
  penyebutnya apa.
- **X6 — README belum diuji di mesin bersih.** Satu-satunya orang yang pernah
  membuktikan README-nya bekerja adalah yang menulisnya, dan itu bukan bukti.
