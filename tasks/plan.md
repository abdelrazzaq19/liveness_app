# Implementation Plan: Liveness Verifier

> Turunan dari [SPEC.md](../SPEC.md) v0.2 · Fase spec-driven: **2/4 (Plan)** · 2026-08-07
> Task list detail ada di [todo.md](todo.md). Dokumen ini menjelaskan **urutan dan alasannya**.

---

## Overview

31 task dalam 6 fase. Prinsip yang mengatur urutannya: **retire the biggest risk first.**

Risiko terbesar project ini bukan logika liveness — itu Go biasa yang bisa di-test tanpa apa pun. Risiko terbesarnya adalah **ONNX Runtime + CGO berjalan di container Debian slim**. Kalau itu gagal, seluruh arsitektur "satu binary tunggal" runtuh dan kita harus mundur ke sidecar Python. Karena itu Fase 2 tidak membangun fitur apa pun — ia hanya membuktikan bahwa satu file JPEG bisa masuk dan bounding box keluar, di dalam Docker. Kalau itu berhasil, sisanya adalah pekerjaan yang bisa diprediksi.

Setiap fase berakhir pada sesuatu yang bisa **dijalankan dan dilihat**, bukan sekadar dikompilasi.

---

## Architecture Decisions

Keputusan yang memengaruhi urutan task, dan alasannya:

1. **Stub pipeline dibangun bersamaan dengan pipeline asli (T13).**
   `biometric.Pipeline` punya dua implementasi: ONNX asli dan stub deterministik yang menurunkan output dari hash gambar. Konsekuensinya seluruh Fase 4 dan 5 — state machine, anti-replay, HTTP, enrollment — bisa dikembangkan dan di-test **tanpa file model sama sekali**. Ini yang membuat test cepat, CI ringan, dan kontributor baru tidak perlu mengunduh 190 MB untuk menjalankan `go test`.

2. **Domain liveness murni, tanpa I/O.**
   `internal/liveness` tidak tahu tentang HTTP, Postgres, atau ONNX. Ia menerima `Face` dan mengembalikan keputusan. Akibatnya T16–T19 bisa dikerjakan **paralel** dengan T14–T15 (storage) dan T9–T12 (model) — tiga jalur yang tidak saling menunggu.

3. **Threshold sebagai konfigurasi sejak T1, bukan sebagai refactor belakangan.**
   Angka di SPEC §5 adalah tebakan dari literatur. Kalibrasi (T30) akan mengubahnya. Kalau threshold di-hardcode dulu lalu "nanti dibikin config", kalibrasi jadi proyek refactor tersendiri. Jadi `internal/config` memuat semuanya sejak task pertama.

4. **Milestone A berhenti di demo webcam yang berfungsi.**
   Bukan di "semua endpoint sudah ada". Checkpoint A hanya lolos kalau Anda benar-benar membuka browser, berkedip, menoleh, dan melihat verdict. Kriteria yang tidak bisa dicurangi.

5. **Postgres dipakai untuk sesi, bukan Redis.**
   Sesi liveness hidup 90 detik dan volumenya rendah di mesin lokal. Menambah container Redis berarti satu dependency lagi untuk keuntungan nol pada skala ini.

6. **Semua build dan test lewat container `dev` (T3).**
   Anda di Windows. CGO + ONNX Runtime + toolchain native di Windows adalah sumber penderitaan yang tidak perlu ditanggung. Container `dev` membuat perintah di mesin Anda identik dengan yang jalan di CI.

---

## Dependency Graph

```
                          ┌─────────────────────────┐
                          │ T1 skeleton + config    │
                          └────────────┬────────────┘
                     ┌─────────────────┼─────────────────┐
                     ▼                 ▼                 ▼
        ┌────────────────────┐  ┌─────────────┐  ┌──────────────────┐
        │ T2 http + healthz  │  │ T7 imaging  │  │ T16 session FSM  │
        └─────────┬──────────┘  └──────┬──────┘  │     + challenge  │
                  ▼                    │         └────────┬─────────┘
        ┌────────────────────┐         │                  │
        │ T3 Docker+compose  │         │                  │
        └─────────┬──────────┘         │                  │
                  ▼                    │                  │
        ┌────────────────────┐         │                  │
        │ T4 modelctl        │         │                  │
        └─────────┬──────────┘         │                  │
                  ▼                    │                  │
        ┌────────────────────┐         │                  │
        │ T5 ORT bootstrap ⚠ │◄────────┘                  │
        └─────────┬──────────┘                            │
                  ▼                                       │
        ┌────────────────────┐                            │
        │ T6 SCRFD detector  │                            │
        └─────────┬──────────┘                            │
                  ▼                                       │
        ┌────────────────────┐   ◄── PROOF: gambar → bbox │
        │ T8 bench CLI       │       di dalam Docker      │
        └─────────┬──────────┘                            │
                  ▼                                       │
     ┌────────────┴──────────────┐                        │
     ▼            ▼         ▼    ▼                        │
   ┌─────┐    ┌──────┐  ┌─────┐ ┌──────┐                  │
   │ T9  │───►│ T10  │  │ T11 │ │ T12  │                  │
   │landm│    │ pose │  │spoof│ │embed │                  │
   └──┬──┘    └──┬───┘  └──┬──┘ └──┬───┘                  │
      └──────────┴─────────┴───────┘                      │
                  ▼                                       │
        ┌────────────────────┐                            │
        │ T13 Pipeline+stub  │                            │
        └─────────┬──────────┘                            │
                  │        ┌──────────────────────┐       │
                  │        │ T14 pg + migrations  │       │
                  │        └──────────┬───────────┘       │
                  │                   ▼                   │
                  │        ┌──────────────────────┐       │
                  │        │ T15 session repo     │       │
                  │        └──────────┬───────────┘       │
                  └───────────┬───────┴───────────────────┘
                              ▼
                  ┌───────────────────────┐
                  │ T17 evaluator         │
                  │ T18 anti-replay       │
                  │ T19 liveness.Service  │
                  └───────────┬───────────┘
                              ▼
                  ┌───────────────────────┐
                  │ T20 liveness handlers │
                  │ T21 demo web UI       │
                  └───────────┬───────────┘
                              ▼
                    ══ MILESTONE A ══
                              ▼
        ┌─────────────────────┴──────────────────────┐
        │ T22 pgvector  T23 minio  T24 token         │
        │              ▼                             │
        │ T25 enrollment.Service → T26 handlers      │
        │              ▼                             │
        │ T27 audit log                              │
        └─────────────────────┬──────────────────────┘
                              ▼
                    ══ MILESTONE B ══
                              ▼
              T28 attacks · T29 obs · T30 calibrate · T31 README
```

Tiga jalur yang **tidak saling menunggu** dan aman dikerjakan paralel:
`T7 imaging` · `T16 session FSM` · `T14→T15 storage`.

---

## Fase dan Checkpoint

### Fase 1 — Walking Skeleton (T1–T3)
Menghasilkan: `docker compose up -d --build` menyalakan service yang menjawab `/healthz`, dan `docker compose run --rm dev go test ./...` hijau. Belum ada fitur, tapi seluruh loop pengembangan sudah hidup.

**Checkpoint 1:** compose naik healthy · `/healthz` 200 dari host Windows · test & lint hijau di container dev · graceful shutdown ≤ 10 detik.

---

### Fase 2 — Inference Spike ⚠ RISIKO TERTINGGI (T4–T8)
Menghasilkan: `go run ./cmd/bench -images testdata/faces` mencetak bounding box dan latensi, **dari dalam container**. Tidak ada API, tidak ada database. Hanya bukti bahwa tumpukan ONNX bekerja.

**Checkpoint 2 — GATE:** SCRFD mendeteksi wajah pada fixture di dalam Docker · session pool lolos `-race` · `modelctl verify` menolak file rusak · p95 deteksi tercatat sebagai baseline.

> Kalau checkpoint ini gagal, **berhenti dan tinjau ulang arsitektur** sebelum menulis satu baris pun kode domain. Opsi mundur ada di tabel risiko.

---

### Fase 3 — Biometric Pipeline (T9–T13)
Menghasilkan: satu gambar masuk → `Face{BBox, Landmarks, Pose, EAR, MAR, SpoofScore, Embedding, Quality}` keluar. Plus stub deterministik yang membuat semua fase berikutnya bisa di-test tanpa model.

**Checkpoint 3:** golden test keempat model lolos · pose pulih ±5° pada rotasi sintetis · dua foto orang sama cosine > 0.6, orang berbeda < 0.3 · stub dan implementasi asli memenuhi interface yang sama.

---

### Fase 4 — Milestone A: Active Liveness (T14–T21)
Menghasilkan: **demo webcam yang berfungsi.**

**Checkpoint A — MILESTONE:** verifikasi A1–A8 di SPEC §9. Yang menentukan: buka `http://localhost:8080/demo`, selesaikan 3 challenge, verdict muncul < 2 detik. Migrasi `up→down→up` bersih. Semua test hijau dengan stub, tanpa model.

---

### Fase 5 — Milestone B: Enrollment & 1:N (T22–T27)
Menghasilkan: daftarkan wajah (wajib lewat sesi liveness yang lolos), cari identitas dari galeri, hapus dengan jejak audit utuh.

**Checkpoint B — MILESTONE:** verifikasi B1–B5 di SPEC §9. Yang menentukan: 10.000 embedding, p95 search < 50 ms, recall@1 ≥ 0.98 vs brute force.

---

### Fase 6 — Hardening (T28–T31)
Menghasilkan: bukti bahwa serangan ditolak, log tidak bocor, threshold terkalibrasi, dan orang lain bisa menjalankan project ini dari nol.

**Checkpoint Selesai:** X1–X6 di SPEC §9 semuanya hijau.

---

## Risks and Mitigations

| # | Risiko | Dampak | Mitigasi | Retired di |
|---|---|---|---|---|
| R1 | `onnxruntime_go` gagal link / crash di Debian slim (CGO, glibc, `libonnxruntime.so` tidak ketemu) | **Tinggi** — meruntuhkan asumsi "satu container" | Diuji di T5, sebelum kode domain apa pun. Fallback berurutan: (a) `debian:bookworm` penuh alih-alih slim, (b) base image ONNX Runtime resmi, (c) mundur ke sidecar Python — perubahan besar, butuh persetujuan Anda | T5 |
| R2 | Lisensi model InsightFace: riset non-komersial | **Tinggi** (legal, bukan teknis) | Dicatat di `models/manifest.json` per model. Open Q#1 masih terbuka. Tidak memblokir pemakaian lokal/riset | Open Q#1 |
| R3 | Threshold di SPEC §5 tebakan, bukan hasil ukur → FRR tinggi pada wajah/pencahayaan nyata | **Tinggi** | Semua threshold env-configurable sejak T1. T30 membangun harness FAR/FRR. Terblokir Open Q#2 & #3 | T30 |
| R4 | `*ort.Session` tidak thread-safe → korupsi memori diam-diam di bawah beban | **Tinggi** | Pool `chan *Session` di T5. `-race` wajib hijau. Test konkurensi eksplisit | T5 |
| R5 | Recall HNSW turun saat galeri membesar | Sedang | T22 mengukur recall@1 vs brute force pada 10k. Kalau < 0.98, naikkan `m`/`ef_construction` | T22 |
| R6 | `getUserMedia` butuh secure context | Sedang | `localhost` **adalah** secure context — demo jalan. Akses via IP LAN tidak akan jalan; didokumentasikan di README | T21 |
| R7 | Toolchain CGO di Windows | Sedang | Semua build/test lewat container `dev`. Go tidak perlu terpasang di host | T3 |
| R8 | Base64 frame @ 6 fps membebani JSON parsing | Rendah | Batas 2 MB/frame, `MaxBytesReader`, pool decoder. WebSocket masuk backlog, bukan v1 | T20 |
| R9 | Kalibrasi ulang saat versi model berganti | Sedang | Versi + SHA-256 dikunci di `manifest.json`. Ganti model masuk kategori "Ask first" di SPEC §8 | — |

---

## Parallelization

| Aman paralel | Harus berurutan | Butuh koordinasi |
|---|---|---|
| T7 imaging ∥ T16 session FSM ∥ T14→T15 storage | T1→T2→T3→T4→T5→T6 (rantai fondasi, tidak bisa dilompati) | T20 handler ∥ T21 demo UI — **bekukan DTO lebih dulu**, lalu paralel |
| T9 ∥ T11 ∥ T12 (model independen setelah T5) | T22→T25→T26 (skema → service → handler) | T13 Pipeline — butuh T9–T12 selesai semua |
| T28 ∥ T29 ∥ T31 | T24 token → T25 enrollment | |

---

## Definition of Done (berlaku untuk setiap task)

Sebuah task tidak dihitung selesai sebelum **semua** ini benar, tanpa kecuali:

- [ ] `docker compose run --rm dev go test ./... -race -count=1` hijau
- [ ] `docker compose run --rm dev golangci-lint run ./...` nol issue
- [ ] `docker compose run --rm dev gofumpt -l .` tidak mengeluarkan apa pun
- [ ] Kode berbahasa Inggris (identifier, komentar, error, log) — SPEC §6
- [ ] Tidak ada threshold/timeout ter-hardcode; semuanya lewat `internal/config`
- [ ] Tidak ada gambar mentah, base64, atau embedding yang masuk log — level apa pun
- [ ] Identifier yang diekspor punya doc comment
- [ ] Perubahan desain tercermin di `SPEC.md` **sebelum** kode diubah

---

## Open Questions

Tercatat di SPEC §10. Status pemblokirannya terhadap plan ini:

| # | Pertanyaan | Memblokir | Bisa jalan tanpa jawaban? |
|---|---|---|---|
| 1 | Lisensi model (riset vs komersial) | T4 (pilihan model) | **Ya** — jalan dulu dengan asumsi riset/personal, dicatat di manifest |
| 2 | Target FAR/FRR | T30 | **Ya** sampai Fase 6 |
| 3 | Dataset kalibrasi | T30, A5, A6 | **Ya** — T28 pakai sampel serangan buatan sendiri dulu |
| 4 | Kebijakan retensi data | T27 (job pembersihan) | **Ya** — audit tetap ditulis, purge menyusul |
| 5 | Skala galeri | T22 (tuning index) | **Ya** — 10k jadi target awal |
| 6 | HTTP vs WebSocket | T20 | **Sudah diputuskan** — HTTP untuk v1 |
| 7 | Otentikasi (API key vs OIDC) | T2 middleware | **Sudah diputuskan** — API key statis untuk lokal |
| 8 | ~~Bahasa kode~~ | — | ✅ Terjawab: kode Inggris, dokumen Indonesia |

**Tidak ada satu pun yang memblokir dimulainya Fase 1.**
