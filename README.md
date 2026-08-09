# Liveness Verifier

Service Go tunggal untuk **verifikasi liveness aktif** (challenge-response) dan
**enrollment wajah + pencarian 1:N**, berjalan sepenuhnya lokal lewat Docker.
Tanpa panggilan ke layanan cloud mana pun.

- Spesifikasi: [SPEC.md](SPEC.md)
- Rencana implementasi: [tasks/plan.md](tasks/plan.md)
- Daftar task: [tasks/todo.md](tasks/todo.md)

## Status

**Fase 1 dari 6 — Walking Skeleton.** Yang sudah ada: konfigurasi, HTTP server,
middleware, `/healthz`, dan image Docker. Belum ada endpoint biometrik.

| Fase | Isi | Status |
|---|---|---|
| 1 | Skeleton, config, Docker | ✅ **selesai & terverifikasi** (Checkpoint 1 lolos) |
| 2 | ONNX Runtime + deteksi wajah | ⬜ |
| 3 | Pipeline biometrik lengkap | ⬜ |
| 4 | Milestone A — active liveness | ⬜ |
| 5 | Milestone B — enrollment & 1:N | ⬜ |
| 6 | Hardening & kalibrasi | ⬜ |

Terverifikasi 2026-08-07: ketiga container `healthy`, test/vet/lint hijau,
graceful shutdown 1,15 detik, image runtime 203 MB. Rinciannya di
[tasks/todo.md](tasks/todo.md).

## Prasyarat

- **Docker Desktop** untuk Windows, dengan Compose v2.24 atau lebih baru.
- Tidak ada yang lain. Go, CGO, dan ONNX Runtime hidup di dalam container.

Cek versi:

```bash
docker compose version
```

### Editor

Buka repo ini **di dalam container**, lewat Dev Containers: di VS Code jalankan
*Dev Containers: Reopen in Container*. Konfigurasinya sudah ada di
[.devcontainer/devcontainer.json](.devcontainer/devcontainer.json).

Ini bukan soal selera. Binding ONNX Runtime dijaga build constraint `cgo`, dan
di Windows tanpa kompiler C `CGO_ENABLED` jatuh ke 0 — seluruh file binding
dikecualikan, lalu editor menandai setiap acuan `ort.*` di kesebelas file
`internal/biometric/onnx/` sebagai kesalahan yang sebenarnya tidak ada. Di
dalam container semuanya build, vet, dan lint dengan bersih.

Kalau lebih suka tetap di host: pasang mingw-w64, lalu `go env -w
CGO_ENABLED=1`. Itu juga berhasil, dengan konsekuensi ada satu toolchain lagi
yang harus dijaga cocok dengan milik container.

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
| `GET` | `/healthz` | — | Probe proses hidup. Dipakai HEALTHCHECK container. |
| `*` | `/v1/*` | `X-API-Key` | Dilindungi API key. Belum ada route di dalamnya. |

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
  pengembangan, bukan sistem eKYC produksi yang lolos audit.
- **Threshold belum terkalibrasi.** FAR/FRR belum diukur terhadap dataset berlabel
  apa pun.
- **Model berlisensi riset non-komersial.** Lihat [models/README.md](models/README.md).
- **Demo webcam (T21) hanya jalan di `localhost`.** `getUserMedia` menuntut secure
  context; membuka lewat IP LAN tanpa HTTPS tidak akan berfungsi.
- **Single-node, single-tenant.** Tidak dirancang untuk skala apa pun di luar satu
  mesin.

## Struktur

```
cmd/server/         Entrypoint HTTP; satu-satunya tempat dependency di-wire
internal/config/    Sumber tunggal seluruh threshold, timeout, dan limit
internal/httpapi/   Lapisan transport — tanpa business logic
deploy/Dockerfile   Multi-stage: ort → builder → runtime, plus stage dev
compose.yaml        Stack lokal: postgres + minio + api (+ dev)
models/             File .onnx — tidak di-commit
tasks/              Rencana dan daftar task
```

Aturan dependensi yang ditegakkan lewat review:

- `internal/liveness` dan `internal/enrollment` tidak boleh mengimpor `net/http`.
- `internal/biometric` tidak boleh mengimpor `onnxruntime_go`; hanya
  `internal/biometric/onnx` yang boleh.
- Wiring hanya terjadi di `cmd/server/main.go`.

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
