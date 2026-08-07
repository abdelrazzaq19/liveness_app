# Spec: Liveness Verifier

> Status: **DRAFT — menunggu review** · Versi 0.1 · 2026-08-06
> Fase spec-driven: **1/4 (Specify)**. Belum boleh masuk Plan/Tasks/Implement sebelum spec ini di-approve.

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
| Inference | `github.com/yalue/onnxruntime_go` + **ONNX Runtime 1.19 (CPU EP)** | Satu container, satu binary. Tidak perlu sidecar Python. |
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
| Test | `testing` + `github.com/stretchr/testify/require` + `testcontainers-go` | Unit murni Go; integrasi terhadap Postgres/MinIO asli. |
| Lint | `golangci-lint` v1.61, `gofumpt` | Gate kualitas di CI dan pre-commit. |

### Model ONNX yang dipakai

| Peran | Model | Output | Ukuran |
|---|---|---|---|
| Face detection | **SCRFD-10GF** (`det_10g.onnx`) | bbox + 5 keypoint | 16,1 MB |
| Dense landmark | **2d106det** (`2d106det.onnx`) | 106 titik 2D | 4,8 MB |
| Head pose | Turunan dari 106 landmark via **PnP** (gonum), model kanonik 3D | yaw / pitch / roll (derajat) | — |
| Face embedding | **ArcFace w600k_r50** (`w600k_r50.onnx`) | vektor 512-dim, L2-normalized | 166,3 MB |
| Passive anti-spoof | **MiniFASNetV2** — ⛔ **belum terselesaikan** | skor real/spoof per frame | ~2 MB |

Ketiga model pertama datang dari satu paket, **`buffalo_l.zip`** (275 MB). InsightFace tidak merilisnya sebagai `.onnx` satuan, jadi `modelctl` mengunduh paketnya lalu mengangkat tiga anggota yang dibutuhkan.

⛔ **MiniFASNetV2 tidak punya rilis ONNX resmi.** Sumber aslinya (Silent-Face-Anti-Spoofing) merilis checkpoint PyTorch `.pth`, bukan ONNX. Ini harus diputuskan sebelum T11 — lihat Open Question #9.

Model **tidak** di-commit ke git dan **tidak** di-bake ke image. Diunduh sekali oleh `cmd/modelctl` ke volume `./models`, diverifikasi dengan SHA-256 yang tercatat di `models/manifest.json`.

---

## 3. Commands

Semua perintah dijalankan dari root project. **Toolchain Go tidak perlu terpasang di Windows** — build dan test berjalan di dalam container `dev` (menghindari kerumitan CGO + ONNX Runtime di Windows native).

```bash
# --- Setup awal (sekali saja) ---
cp deploy/.env.example .env
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
docker compose run --rm dev go run ./cmd/modelctl verify       # cek integritas model
docker compose run --rm dev go run ./cmd/bench -images testdata/faces  # benchmark latensi
```

**Port yang dipakai:** `8080` API+demo · `5432` Postgres · `9000`/`9001` MinIO API/Console.

---

## 4. Project Structure

```
liveness-verifier/
├── SPEC.md                        # dokumen ini — source of truth
├── README.md                      # quickstart 5 menit
├── go.mod / go.sum
├── .env.example                   # template konfigurasi (TIDAK berisi secret asli)
│
├── cmd/
│   ├── server/main.go             # entrypoint HTTP server; wiring dependency saja
│   ├── modelctl/main.go           # download + verifikasi checksum model ONNX
│   └── bench/main.go              # benchmark latensi inference offline
│
├── internal/
│   ├── config/                    # struct Config + parsing env + validasi saat boot
│   │
│   ├── httpapi/                   # LAPISAN TRANSPORT — tidak ada business logic
│   │   ├── router.go              # definisi rute chi
│   │   ├── middleware.go          # request ID, recover, timeout, API key, rate limit
│   │   ├── dto.go                 # struct request/response JSON + validasi
│   │   ├── errors.go              # mapping error domain → HTTP status + body
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
│   │   └── threshold.go           # kalibrasi ambang cosine similarity
│   │
│   ├── biometric/                 # PORT — interface, bebas dari ONNX
│   │   ├── types.go               # Face, BBox, Landmarks, Pose, Embedding
│   │   ├── ports.go               # Detector, Landmarker, AntiSpoofer, Embedder
│   │   ├── pose.go                # PnP: 106 landmark → yaw/pitch/roll
│   │   ├── metrics.go             # EAR (blink), MAR (buka mulut)
│   │   └── onnx/                  # ADAPTER — implementasi onnxruntime_go
│   │       ├── runtime.go         # init/teardown ORT env, session pool
│   │       ├── detector_scrfd.go
│   │       ├── landmarker_2d106.go
│   │       ├── antispoof_minifas.go
│   │       └── embedder_arcface.go
│   │
│   ├── imaging/                   # decode, EXIF, resize, align 5-point, normalisasi
│   │   ├── decode.go
│   │   ├── align.go               # similarity transform ke template 112x112
│   │   ├── quality.go             # blur (Laplacian var), brightness, ukuran wajah
│   │   └── phash.go               # perceptual hash untuk deteksi frame duplikat
│   │
│   └── storage/
│       ├── postgres/              # ADAPTER — repository, pgvector codec
│       │   ├── db.go
│       │   ├── session_repo.go
│       │   ├── face_repo.go       # termasuk query HNSW 1:N
│       │   └── audit_repo.go
│       └── objstore/              # ADAPTER — MinIO
│           └── minio.go
│
├── migrations/                    # goose SQL, penomoran urut
│   ├── 00001_init_extensions.sql
│   ├── 00002_liveness_sessions.sql
│   ├── 00003_faces_pgvector.sql
│   └── 00004_audit_log.sql
│
├── web/                           # di-embed via //go:embed
│   ├── index.html                 # demo: webcam → challenge → verdict
│   ├── app.js
│   └── style.css
│
├── models/                        # .onnx + manifest.json  (DI-GITIGNORE, kecuali manifest)
│   └── manifest.json              # nama, URL, SHA-256, lisensi tiap model
│
├── testdata/                      # gambar fixture untuk unit test (wajah sintetis/CC0)
│   ├── golden/                    # golden file output detector/embedder
│   └── attacks/                   # sampel print & replay attack untuk regression
│
├── tests/integration/             # build tag `integration`, pakai testcontainers
│
└── deploy/
    ├── Dockerfile                 # multi-stage: builder (CGO) → runtime slim
    ├── Dockerfile.dev             # image dev: go, golangci-lint, goose, gofumpt
    ├── docker-compose.yml
    └── .env.example
```

**Aturan dependensi (ditegakkan lewat review, bukan hanya konvensi):**

```
httpapi ──→ liveness / enrollment ──→ biometric (port) ──→ biometric/onnx (adapter)
                     │                      ▲
                     └──→ storage/*  ───────┘   imaging
```

- `internal/biometric` **tidak boleh** mengimpor `onnxruntime_go`. Hanya `internal/biometric/onnx` yang boleh.
- `internal/liveness` dan `internal/enrollment` **tidak boleh** mengimpor `net/http` atau `chi`.
- Wiring semua dependency terjadi **hanya** di `cmd/server/main.go`.

---

## 5. API Surface

Semua endpoint di-prefix `/v1`, membutuhkan header `X-API-Key`. Body JSON, gambar dikirim sebagai base64 di field `frame` (batas 2 MB per frame).

### Liveness

| Method | Path | Fungsi |
|---|---|---|
| `POST` | `/v1/liveness/sessions` | Buat sesi. Response: `session_id`, `nonce`, daftar `challenges` (urutan diacak), `expires_at`. |
| `POST` | `/v1/liveness/sessions/{id}/frames` | Kirim 1 frame + `seq` + `client_ts`. Response: challenge aktif, progres, `advanced` (bool), alasan jika ditolak. |
| `POST` | `/v1/liveness/sessions/{id}/complete` | Finalisasi. Response: `verdict` (`PASSED` / `FAILED`), skor, `liveness_token` (JWT singkat, dipakai untuk enroll). |
| `GET` | `/v1/liveness/sessions/{id}` | Status sesi (tanpa data biometrik mentah). |

### Faces

| Method | Path | Fungsi |
|---|---|---|
| `POST` | `/v1/faces` | Enroll. Wajib menyertakan `liveness_token` yang valid & belum terpakai. |
| `POST` | `/v1/faces/search` | **1:N** — satu gambar → top-K kandidat + `similarity`, `match` (bool). |
| `POST` | `/v1/faces/verify` | **1:1** — gambar vs `subject_id` tertentu. *(Turunan gratis dari 1:N — satu handler tipis. Coret kalau tidak perlu.)* |
| `GET` | `/v1/faces/{subject_id}` | Metadata subject (tanpa embedding mentah). |
| `DELETE` | `/v1/faces/{subject_id}` | Hapus subject: embedding + objek di MinIO + tandai audit. Hard delete. |

### Operasional

`GET /healthz` (liveness probe) · `GET /readyz` (cek DB, MinIO, model ter-load) · `GET /demo` (UI).

### Anti-replay: lapisan pertahanan

Sesi dianggap valid hanya jika **semua** ini terpenuhi:

1. **Challenge acak** — 3 challenge dipilih dan diurutkan acak per sesi; video rekaman tidak akan cocok dengan urutan yang belum diketahui.
2. **Nonce + sequence** — `seq` harus naik monoton; frame duplikat/ mundur ditolak.
3. **Batas waktu** — sesi kedaluwarsa dalam 90 detik; tiap challenge maksimal 20 detik.
4. **Dedup perceptual hash** — dua frame dengan Hamming distance pHash < 5 dianggap frame yang sama (indikasi replay statis).
5. **Passive anti-spoof per frame** — MiniFASNetV2; frame dengan skor spoof di atas ambang langsung menggagalkan sesi.
6. **Konsistensi identitas lintas frame** — embedding ArcFace tiap frame kunci harus cosine ≥ 0.70 terhadap frame pertama. Mencegah pergantian wajah di tengah sesi.

### Threshold awal (bukan hasil kalibrasi — lihat §9)

| Parameter | Nilai awal | Sumber |
|---|---|---|
| Skor deteksi wajah minimum | 0.60 | default SCRFD |
| Lebar wajah minimum | 112 px | ukuran input ArcFace |
| EAR blink (mata tertutup) | < 0.21 selama ≥ 2 frame berturut | literatur Soukupová & Čech |
| Yaw untuk tengok kiri/kanan | \|yaw\| > 25° | ditetapkan, perlu kalibrasi |
| MAR buka mulut | > 0.55 | ditetapkan, perlu kalibrasi |
| Skor passive liveness minimum | 0.80 | ditetapkan, perlu kalibrasi |
| Cosine match 1:N | ≥ 0.42 | rekomendasi umum buffalo_l |
| Blur minimum (Laplacian variance) | > 80 | ditetapkan, perlu kalibrasi |

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
| **Golden** | `testdata/golden/` | Output detector/landmarker/embedder terhadap gambar fixture tetap. Mendeteksi regresi diam-diam saat ganti versi model. | `go test ./internal/biometric/...` |
| **Integrasi** | `tests/integration/` (tag `integration`) | Postgres asli (pgvector, HNSW recall) + MinIO asli via testcontainers. Migrasi naik-turun. | `go test -tags=integration ./tests/integration/...` |
| **API / kontrak** | `tests/integration/api_test.go` | Server penuh dengan pipeline biometrik **stub deterministik**. Flow sesi lengkap, kode error, bentuk JSON. | idem |
| **Adversarial** | `tests/integration/attack_test.go` | Print attack, replay video, frame duplikat, seq mundur, identitas berganti di tengah sesi — semua **harus** ditolak. | idem |
| **Benchmark** | `*_bench_test.go` | Latensi per tahap inference, throughput 1:N pada 10k embedding. | `go test -bench=. -benchmem ./...` |

**Target coverage:**

- `internal/liveness`, `internal/enrollment`, `internal/biometric` (kecuali `onnx/`): **≥ 80%**
- Keseluruhan repo: **≥ 70%**
- `internal/biometric/onnx/`: dikecualikan dari target coverage (butuh model asli); dijaga oleh golden test.

**Aturan test:**

1. Semua test **harus** bisa jalan tanpa file model `.onnx` kecuali yang bertag `models`. Stub pipeline adalah default.
2. Test **tidak boleh** menyentuh jaringan luar. Model dan container di-pin dan lokal.
3. **Tidak ada foto wajah orang asli** di `testdata/`. Gunakan wajah sintetis atau dataset berlisensi CC0/publik. Ini bukan preferensi — ini batas hukum data biometrik.
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
- Menambah container ke `docker-compose.yml`.
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
| A2 | `GET /readyz` mengembalikan 200 dengan status DB, MinIO, dan 4 model ter-load. | `curl -s localhost:8080/readyz \| jq` |
| A3 | Demo di `http://localhost:8080/demo` menuntaskan sesi 3-challenge memakai webcam, verdict muncul < 2 detik setelah frame terakhir. | Uji manual, direkam di README. |
| A4 | Latensi inference per frame **p95 < 150 ms** pada CPU 4 core. | `go test -bench=BenchmarkPipeline -benchmem` |
| A5 | Print attack (foto dicetak / ditampilkan di layar HP) ditolak. | `attack_test.go` + uji manual. |
| A6 | Replay attack (video rekaman orang yang sama) gagal karena urutan challenge acak. | `attack_test.go` |
| A7 | Frame duplikat, `seq` mundur, dan pergantian identitas di tengah sesi ditolak dengan kode error yang tepat. | `attack_test.go` |
| A8 | Sesi kedaluwarsa otomatis pada 90 detik; sesi kedaluwarsa dibersihkan dari DB. | Uji integrasi dengan fake clock. |

### Milestone B — Enrollment & 1:N

| # | Kriteria | Cara verifikasi |
|---|---|---|
| B1 | Enrollment **menolak** request tanpa `liveness_token` yang valid dan belum terpakai. | `api_test.go` |
| B2 | 1:N search pada galeri 10.000 embedding: **p95 < 50 ms** dengan index HNSW. | Uji integrasi dengan embedding sintetis. |
| B3 | Recall@1 index HNSW ≥ 0.98 dibanding brute force exact search pada dataset yang sama. | Uji integrasi. |
| B4 | `DELETE /v1/faces/{id}` menghapus embedding **dan** objek MinIO, serta menyisakan baris audit. | Uji integrasi. |
| B5 | Setiap keputusan verifikasi menghasilkan baris audit yang dapat ditelusuri lengkap dengan skor dan referensi artefak. | Uji integrasi. |

### Lintas milestone

| # | Kriteria | Cara verifikasi |
|---|---|---|
| X1 | `go test ./... -race` lolos bersih. | CI + lokal. |
| X2 | Coverage domain ≥ 80%, repo ≥ 70%. | `go tool cover -func` |
| X3 | `golangci-lint run ./...` nol issue. | CI + lokal. |
| X4 | Test khusus membuktikan tidak ada gambar/embedding yang bocor ke log. | Test yang menangkap output `slog` dan memindainya. |
| X5 | Migrasi `up` lalu `down` lalu `up` kembali berhasil pada database kosong. | Uji integrasi. |
| X6 | README memungkinkan orang lain menjalankan project ini dari nol dalam < 10 menit. | Uji ulang di mesin bersih. |

---

## 10. Open Questions

Perlu jawaban Anda — beberapa memblokir kalibrasi, sisanya bisa diputuskan sambil jalan:

1. **Lisensi model.** Model InsightFace pre-trained berlisensi riset non-komersial. Apakah project ini akan tetap di ranah riset/personal, atau perlu jalur model berlisensi komersial (mis. melatih ulang, atau memakai model berlisensi permisif dengan akurasi lebih rendah)? — *Memblokir keputusan model.*

2. **Target FAR/FRR.** Berapa target False Accept Rate dan False Reject Rate? Tanpa angka, threshold di §5 hanyalah tebakan berdasar literatur. Standar industri eKYC: FAR ≤ 0.01%, FRR ≤ 5%. — *Memblokir kalibrasi threshold.*

3. **Dataset kalibrasi.** Akan pakai apa untuk mengukur? CelebA-Spoof / CASIA-SURF / rekaman sendiri? Perlu diputuskan sebelum Milestone A8. — *Memblokir A5/A6.*

4. **Retensi data.** Berapa lama artefak frame dan baris audit disimpan? Perlukah job pembersihan otomatis? UU PDP Indonesia mensyaratkan batas retensi eksplisit untuk data biometrik.

5. **Skala galeri.** Berapa perkiraan jumlah subject terdaftar? 10k dan 10 juta membutuhkan strategi index yang berbeda secara fundamental (HNSW in-memory vs sharding).

6. **Streaming frame.** Spec ini memakai HTTP POST per frame (~5–8 fps). WebSocket akan lebih halus dan hemat overhead. Cukupkah HTTP untuk v1? — *Rekomendasi saya: ya, HTTP dulu; WebSocket masuk backlog.*

7. **Otentikasi.** Static API key sudah cukup untuk pemakaian lokal? Atau perlu langsung OIDC/mTLS?

8. ~~**Bahasa dokumentasi & kode.**~~ ✅ **TERJAWAB (2026-08-07):** kode berbahasa Inggris, dokumentasi berbahasa Indonesia. Lihat baris "Bahasa" di §6.

9. **Model anti-spoof pasif.** ⛔ **BARU — ditemukan saat T4.** MiniFASNetV2 tidak punya rilis ONNX resmi; upstream hanya merilis checkpoint PyTorch. Tiga jalan keluar:

   | Opsi | Konsekuensi |
   |---|---|
   | **(a)** Konversi `.pth` → ONNX sendiri | Butuh Python + PyTorch sekali saat setup, lalu hasil konversinya di-pin di manifest. Menambah langkah setup yang tidak bisa dilakukan `modelctl` sendirian. |
   | **(b)** Pakai model anti-spoof ONNX dari pihak ketiga | Cepat, tapi asal-usul dan lisensinya lebih sulit dipertanggungjawabkan. |
   | **(c)** Tunda — andalkan pertahanan lain dulu | Lima dari enam lapis anti-replay di §5 tetap jalan tanpa model ini. Yang hilang adalah deteksi serangan cetak/layar dari satu frame, dan itu justru serangan paling umum. |

   *Memblokir T11.* Tidak memblokir T5–T10. Rekomendasi saya: **(a)**, dengan skrip konversi terpisah yang dijalankan sekali dan hasilnya di-pin — asal-usulnya jelas dan hasilnya tetap reproducible.

---

## 11. Riwayat Revisi

| Versi | Tanggal | Perubahan |
|---|---|---|
| 0.1 | 2026-08-06 | Draft awal. Scope: active liveness + enrollment/1:N, Go + ONNX Runtime, REST + demo UI, Postgres/pgvector + MinIO. |
| 0.2 | 2026-08-07 | **Spec di-approve.** Open question #8 ditutup: kode Inggris, dokumen Indonesia. Contoh di §6 ditulis ulang. Lanjut ke Phase 2 (Plan). |
