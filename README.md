# Liveness Verifier

Service Go tunggal untuk **verifikasi liveness aktif** (challenge-response) dan
**enrollment wajah + pencarian 1:N**, berjalan sepenuhnya lokal lewat Docker.
Tanpa panggilan ke layanan cloud mana pun.

- Kontrak API: [docs/API.md](docs/API.md)
- Struktur project: [docs/STRUCTURE.md](docs/STRUCTURE.md)
- Menjalankan deploy: [docs/DEPLOY.md](docs/DEPLOY.md)
- Spesifikasi: [SPEC.md](SPEC.md)
- Rencana implementasi: [tasks/plan.md](tasks/plan.md)
- Daftar task: [tasks/todo.md](tasks/todo.md)

## Status

**Fase 6 dari 6 — Hardening.** Seluruh jalur liveness dan enrollment berjalan.
Yang belum: kalibrasi ambang, dan verifikasi end-to-end dengan wajah sungguhan.

| Fase | Isi | Status |
|---|---|---|
| 1 | Skeleton, config, Docker | ✅ selesai |
| 2 | ONNX Runtime + deteksi wajah | ✅ selesai |
| 3 | Pipeline biometrik lengkap | ✅ selesai |
| 4 | Milestone A — active liveness | ✅ kode selesai, ⚠ belum diverifikasi webcam |
| 5 | Milestone B — enrollment & 1:N | ✅ selesai |
| 6 | Hardening & kalibrasi | ⚠ sebagian — T30 diblokir |

Angka yang terukur, bukan diperkirakan:

| | |
|---|---|
| Pencarian 1:N, 10.000 wajah | p50 4,3 ms · p95 6,4 ms · recall@1 1,000 |
| Pipeline per frame biasa | p95 149 ms pada input detektor 320 |
| Pipeline per frame kunci | p95 796 ms (embedder 71% dari totalnya) |
| Suite serangan | 10 serangan ditolak |

Rinciannya di [tasks/todo.md](tasks/todo.md) dan [tasks/baseline.md](tasks/baseline.md).

### ⚠ Tiga hal yang harus dibaca sebelum memakai ini

1. **Anti-spoof pasif sedang dimatikan.** Konversi MiniFASNetV2 yang dipakai
   memberi skor ~0,006 untuk wajah asli terhadap ambang 0,80 — ia menolak
   **setiap** subjek sungguhan. Jalur Go sudah dibuktikan menghasilkan logits
   identik dengan PyTorch aslinya, jadi ini bukan bug implementasi. Selama
   `LV_LIVENESS_ANTISPOOF_ENFORCE=false`, **foto cetak dan replay layar tidak
   diblokir.** Server memperingatkan setiap boot.

2. **Ambang belum terkalibrasi.** Sebagian berasal dari satu sesi dengan satu
   orang, satu kamera, 34 frame — cukup membuktikan angka lama salah, sama
   sekali tidak cukup menyebut yang baru benar.

3. **Belum ada satu sesi pun yang lolos penuh dengan wajah sungguhan.**
   Checkpoint A3 menunggu seseorang dengan webcam.

## Prasyarat

- **Docker Desktop** untuk Windows, dengan Compose v2.24 atau lebih baru.
- Tidak ada yang lain. Go, CGO, dan ONNX Runtime hidup di dalam container.

Cek versi:

```bash
docker compose version
```

### Editor

Binding ONNX Runtime dijaga build constraint `cgo`. Tanpa kompiler C,
`CGO_ENABLED` jatuh ke 0, seluruh file binding dikecualikan, dan editor
menandai **setiap** import di `internal/biometric/onnx/` sebagai kesalahan —
termasuk `math` dari stdlib, karena paket yang gagal memuat dependensinya tidak
bisa di-type-check sama sekali. Kodenya sendiri tidak salah.

Dua cara memperbaikinya. Pilih satu.

**Bekerja di Windows.** Pasang toolchain-nya sekali:

```bash
winget install --id BrechtSanders.WinLibs.POSIX.UCRT
```

lalu `go env -w CGO_ENABLED=1`, lalu **restart language server** (Ctrl+Shift+P
→ *Go: Restart Language Server*) — gopls membaca `go env` sekali saat mulai.
Konsekuensinya ada kompiler kedua yang versinya bisa berbeda dari milik
container; build resmi tetap lewat Docker.

**Atau bekerja di dalam container.** Ctrl+Shift+P → *Dev Containers: Reopen in
Container*. Tidak ada yang dipasang di host, dan lingkungannya sama persis
dengan gerbang kualitas. Konfigurasinya di
[.devcontainer/devcontainer.json](.devcontainer/devcontainer.json).

Apa pun pilihannya, sebutkan tag `models` dan `integration` ke gopls, kalau
tidak test di bawahnya tidak terlihat dan tampak seperti kode mati sampai
gerbang kualitas menemukannya. Devcontainer sudah mengaturnya; untuk host,
`.vscode/settings.json` (tidak ikut git):

```json
{ "gopls": { "build.buildFlags": ["-tags=models,integration"] } }
```

## Menjalankan

**1. Buat konfigurasi**

```bash
cp .env.example .env
```

Buka `.env` dan ganti setiap nilai `<ganti>`. Untuk membangkitkan secret:

```bash
openssl rand -hex 32
```

Yang wajib diisi: `POSTGRES_PASSWORD`, `MINIO_ROOT_PASSWORD`, `LV_API_KEYS`,
`LV_TOKEN_SECRET`, dan bagian password di dalam `LV_DATABASE_URL`.

**2. Nyalakan**

```bash
docker compose up -d --build
```

Build pertama mengunduh ONNX Runtime dan image dasar, perkiraan 3–5 menit.

**3. Cek**

```bash
curl -s http://localhost:8080/healthz
```

Jawaban yang diharapkan: `{"status":"ok"}`

```bash
docker compose ps
```

`postgres`, `minio`, dan `api` harus berstatus `healthy`.

## Perintah pengembangan

Semua dijalankan lewat container `dev`, jadi Go tidak perlu terpasang di host.

```bash
docker compose run --rm dev go test ./... -race -count=1
```

```bash
docker compose run --rm dev golangci-lint run ./...
```

```bash
docker compose run --rm dev gofumpt -l -w .
```

```bash
docker compose logs -f api
```

Menghentikan (volume data tetap ada):

```bash
docker compose down
```

Menghentikan **dan menghapus seluruh data** — Postgres, MinIO, cache build:

```bash
docker compose down -v
```

## Endpoint saat ini

| Method | Path | Auth | Keterangan |
|---|---|---|---|
| `GET` | `/healthz` | — | Proses hidup. Dipakai HEALTHCHECK container. |
| `GET` | `/readyz` | — | Bisa melayani: database, skema, object store. 503 kalau tidak. |
| `POST` | `/v1/liveness/sessions` | `X-API-Key` | Buka sesi. Mengembalikan `session_id` + `nonce`. |
| `POST` | `/v1/liveness/sessions/{id}/frames` | `X-Session-Nonce` | Kirim satu frame. |
| `POST` | `/v1/liveness/sessions/{id}/complete` | `X-Session-Nonce` | Putusan. Mengembalikan token bila lolos. |
| `GET` | `/v1/liveness/sessions/{id}` | `X-Session-Nonce` | Status sesi. |
| `POST` | `/v1/faces` | `X-API-Key` | Daftarkan wajah. Butuh token liveness. |
| `POST` | `/v1/faces/search` | `X-API-Key` | Cari identitas dari sebuah wajah. |
| `DELETE` | `/v1/faces` | `X-API-Key` | Hapus semua wajah satu subjek. `subject_id` di **body**. |

**Dua kredensial, dan keduanya tidak saling menggantikan.** API key adalah
kredensial **operator**: ia membuka sesi dan menulis ke galeri. Nonce adalah
kredensial **subjek**: ia mengoperasikan satu sesi dan tidak bisa apa-apa
selainnya. Menaruh API key di browser subjek adalah cara ia berakhir di tempat
yang tidak semestinya.

### Alur yang dimaksudkan

```
backend Anda            service ini              browser subjek
     │                       │                         │
     ├── POST /sessions ────►│  (X-API-Key)            │
     │◄── session_id, nonce ─┤                         │
     ├── kirim id + nonce ───┼────────────────────────►│
     │                       │◄── POST /frames ────────┤  (X-Session-Nonce)
     │                       │──► instruksi berikutnya │
     │                       │◄── POST /complete ──────┤
     │◄── token ─────────────┼─────────────────────────┤
     ├── POST /faces ───────►│  (X-API-Key + token)    │
     │                       │  wajah dicocokkan       │
     │                       │  dengan capture         │
     │                       │  yang terverifikasi     │
```

Token itu sekali pakai, berumur pendek, dan terikat pada satu sesi. Ia
membuktikan capture-nya hidup — dan wajah yang dikirim bersamanya **dicocokkan
kembali** dengan wajah yang terverifikasi, karena tanpa itu seseorang bisa lolos
liveness dengan wajahnya sendiri lalu mendaftarkan foto orang lain.

Semua respons error memakai bentuk yang sama:

```json
{
  "error": {
    "code": "unauthorized",
    "message": "a valid X-API-Key header is required",
    "request_id": "9f2c1a7b3e4d5c60"
  }
}
```

## Konfigurasi

Seluruh tunable dibaca dari environment. Daftar lengkapnya, beserta default dan
penjelasannya, ada di [.env.example](.env.example).

Yang perlu diketahui:

- **`LV_PIPELINE_MODE`** — default `stub`. Pipeline biometrik tiruan yang
  deterministik, tidak butuh file model sama sekali. Ganti ke `onnx` setelah
  model diunduh (T4). Default `stub` inilah yang membuat `go test ./...` berjalan
  di direktori `models/` yang kosong.
- **Threshold biometrik** — semuanya adalah estimasi dari literatur, **bukan hasil
  pengukuran**. Lihat [tasks/todo.md](tasks/todo.md) T30. Jangan mempercayainya
  untuk keputusan yang serius sebelum dikalibrasi.
- **Secret** — dibungkus tipe yang menolak mencetak dirinya sendiri, jadi tidak
  ikut terbawa ke log meskipun struct config di-`%+v`.

Konfigurasi yang salah gagal saat boot, dan melaporkan **semua** kesalahannya
sekaligus, bukan satu per restart.

## Batasan

Baca ini sebelum menaruh harapan yang tidak semestinya:

- **Bukan sistem tersertifikasi ISO/IEC 30107-3 (PAD).** Ini build riset dan
  pengembangan, bukan sistem eKYC produksi yang lolos audit. Selama anti-spoof
  pasif dimatikan, sistem ini bahkan tidak memblokir foto cetak.
- **Threshold belum terkalibrasi.** FAR/FRR belum diukur terhadap dataset berlabel
  apa pun. Beberapa angka berasal dari satu orang di satu ruangan.
- **Foto cetak dan replay layar tidak ada dalam suite regresi serangan.**
  Keduanya butuh capture wajah asli berlabel yang tidak boleh disimpan repo ini.
  Sepuluh serangan lain diuji setiap kali test dijalankan.
- **Enrollment mempercayai `subject_id` dari pemanggil.** Service ini tidak
  punya pendapat tentang siapa yang berhak memakai identitas mana; itu urusan
  backend yang memegang API key.
- **Model berlisensi riset non-komersial.** Lihat [models/README.md](models/README.md).
- **Demo webcam (T21) hanya jalan di `localhost`.** `getUserMedia` menuntut secure
  context; membuka lewat IP LAN tanpa HTTPS tidak akan berfungsi.
- **Single-node, single-tenant.** Tidak dirancang untuk skala apa pun di luar satu
  mesin.

## Struktur

```
cmd/server/            Entrypoint; satu-satunya tempat dependency di-wire
cmd/modelctl/          Unduh, verifikasi, dan pin model
cmd/bench/             Ukur latensi pipeline

internal/config/       Sumber tunggal seluruh threshold, timeout, dan limit
internal/biometric/    Domain: tipe, port, metrik, pose, pipeline
internal/biometric/onnx/   Satu-satunya paket yang mengimpor onnxruntime_go
internal/biometric/stub/   Pipeline tiruan deterministik, tanpa file model
internal/imaging/      Decode, gerbang kualitas, alignment, pHash
internal/liveness/     Sesi, challenge, evaluator, anti-replay
internal/enrollment/   Galeri, token sekali pakai, audit
internal/httpapi/      Transport — tanpa business logic
internal/storage/      Adapter Postgres dan MinIO

migrations/            Skema, naik dan turun
tests/attack/          Suite regresi serangan, jalan di test biasa
tests/integration/     Terhadap Postgres dan MinIO sungguhan
tools/antispoof/       Konversi checkpoint PyTorch ke ONNX
```

Aturan dependensi, dan alasannya:

- **`internal/liveness` dan `internal/enrollment` tidak mengimpor `net/http`.**
  Domain yang tahu tentang HTTP akan mulai membuat keputusan berdasarkan status
  code.
- **`internal/biometric` tidak mengimpor `onnxruntime_go`**; hanya
  `internal/biometric/onnx`. Itu yang membuat pipeline stub mungkin, dan pipeline
  stub itu yang membuat suite serangan bisa jalan tanpa file model.
- **`internal/liveness` tidak tahu `internal/enrollment` ada.** Ketergantungannya
  menunjuk ke dalam: enrollment mendeklarasikan antarmuka kecil untuk apa yang
  dibutuhkannya dari sesi, dan `cmd/server` yang menyambungkan.
- **Wiring hanya di `cmd/server`.** Ia satu-satunya yang tahu setiap paket ada.

## Pemecahan masalah

**`POSTGRES_PASSWORD belum diset`** — Anda melewatkan `cp .env.example .env`.

**MinIO tidak pernah `healthy`** — healthcheck memakai `mc ready local`. Kalau
tag image yang dipakai tidak menyertakan `mc`, ganti healthcheck-nya di
`compose.yaml` menjadi `curl -fsS http://localhost:9000/minio/health/live`.

**Build gagal saat mengunduh ONNX Runtime** — periksa `ORT_VERSION` di
`deploy/Dockerfile` masih ada di [rilis onnxruntime](https://github.com/microsoft/onnxruntime/releases).

**`libonnxruntime` tidak ditemukan** — build image runtime memang sengaja gagal
kalau `ldconfig -p` tidak menemukannya. Ini risiko R1 di
[tasks/plan.md](tasks/plan.md), dan gagal saat build jauh lebih baik daripada
gagal saat request pertama.

## Verifikasi akhir

Dijalankan 2026-08-10, seluruhnya di dalam container:

| Yang diperiksa | Hasil |
|---|---|
| `gofumpt -l .` | tidak ada berkas terlapor |
| `go vet` × 3 tag (default, `models`, `integration`) | bersih |
| `golangci-lint run ./...` | bersih |
| Unit + serangan, `-race` | 10 paket lulus |
| Integrasi (Postgres + MinIO sungguhan) | lulus, 124 dtk |
| Test bertag `models` terhadap `.onnx` asli | lulus, 47 dtk |
| `docker compose down` lalu `up --build` dari nol | ketiga container `healthy` |
| `/healthz` | `{"status":"ok","pipeline":"onnx"}` |
| `/readyz` | `{"status":"ready"}` |
| `/readyz` saat Postgres dimatikan | `503`, sementara `/healthz` tetap `200` |
| Graceful shutdown | 1,18 detik, `shutdown complete` tercatat |

Yang **tidak** diverifikasi, dan tidak bisa diverifikasi tanpa seseorang di
depan kamera:

- Satu sesi liveness lengkap yang lolos dengan wajah sungguhan (Checkpoint A3).
- Enrollment dan pencarian 1:N end-to-end dengan wajah sungguhan (Checkpoint B).
- Penolakan foto cetak dan replay layar (Checkpoint A5/A6) — yang juga menunggu
  anti-spoof pasif bisa dinyalakan kembali.
