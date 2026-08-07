# Baseline Latensi

> Diukur 2026-08-07 dengan `cmd/bench` · dirujuk oleh kriteria **A4** di [SPEC.md](../SPEC.md#9-success-criteria)

## Lingkungan pengukuran

| | |
|---|---|
| Host | Windows 11, Docker Desktop (VM) |
| Container | `debian:bookworm`, 8 core terlihat |
| ONNX Runtime | 1.28.0, CPU execution provider |
| `IntraOpThreads` | 0 (auto) |
| Pool | 1 sesi, concurrency 1 |
| Input | 4 adegan sintetis 480×640, 15 ulangan = **60 sampel** per konfigurasi |
| Warm-up | 8 inferensi, tidak diukur |

Perintahnya:

```bash
docker compose run --rm dev go run ./cmd/bench -synthetic 4 -repeat 15 -warmup 8 -model det_500m.onnx -size 640
```

## Hasil: detektor saja

| Model | Input | p50 | p95 |
|---|---|---|---|
| **SCRFD-500M** | 640 | **131,9 ms** | 256,6 ms |
| **SCRFD-500M** | 320 | **60,6 ms** | **115,3 ms** |
| SCRFD-10GF | 640 | 985,9 ms | 1269,5 ms |
| SCRFD-10GF | 320 | 305,8 ms | 477,2 ms |

## ⚠️ Koreksi terhadap angka yang dilaporkan sebelumnya

Pengukuran pertama di T6 melaporkan SCRFD-500M @ 640 sebesar **73 ms**. **Angka itu
tidak bisa dipercaya.** Diambil dengan `-benchtime 5x` dan `10x` — 5 sampai 10
sampel — dan kebetulan menangkap jendela cepat.

Dengan 60 sampel, angka sebenarnya sekitar **132 ms p50**. Bench CLI dan
benchmark Go sekarang sepakat dalam 0,6 ms satu sama lain pada beban yang sama,
jadi keduanya benar; ukuran sampelnya yang salah.

**Pelajarannya, dan sebabnya dicatat di sini:** lingkungan ini bising. Rentang
min-ke-p99 mencapai 3,3× pada beban identik. Kesimpulan apa pun dari kurang dari
beberapa puluh sampel di mesin ini adalah tebakan yang menyamar sebagai
pengukuran.

**Keputusan ganti model tetap benar.** SCRFD-500M @ 640 (132 ms) masih lebih cepat
daripada SCRFD-10GF @ 320 (306 ms), jadi resolusi penuh dengan model ringan tetap
mengalahkan seperempat resolusi dengan model berat. Perbandingannya bertahan
karena keduanya diukur di kondisi yang sama; hanya besaran mutlaknya yang bergeser.

## Pipeline penuh — diukur di T13

Keempat model dirangkai, per tahap. Perintahnya:

```bash
docker compose run --rm dev go run ./cmd/bench -full -synthetic 4 -repeat 10 -warmup 5 -size 320
```

### Input detektor 320

| Tahap | p50 | p95 | p99 |
|---|---|---|---|
| Gerbang kualitas | 7,6 ms | 14,0 ms | 16,1 ms |
| Detektor | 40,9 ms | 64,7 ms | 94,0 ms |
| Landmarker | 24,5 ms | 56,3 ms | 67,0 ms |
| Anti-spoof | 15,5 ms | 30,3 ms | 68,5 ms |
| **Embedder** | **434,7 ms** | **673,0 ms** | 714,3 ms |
| TOTAL (frame kunci) | 534,9 ms | 796,4 ms | 804,3 ms |
| **Per frame** (tanpa embedder) | **89,5 ms** | **149,1 ms** | 169,3 ms |

### Input detektor 640

| Tahap | p50 | p95 |
|---|---|---|
| Detektor | 112,9 ms | 196,5 ms |
| TOTAL (frame kunci) | 621,4 ms | 765,4 ms |
| **Per frame** (tanpa embedder) | **166,3 ms** | **242,7 ms** |

### Yang dikatakan angka-angka ini

**Embedder mendominasi total: ~450 ms, 71% dari seluruh pipeline.** Dan biayanya
**tidak berubah** dengan resolusi detektor — input-nya tetap crop 112×112 hasil
alignment. Menurunkan resolusi detektor tidak menolong frame kunci sama sekali.

Tapi embedder hanya jalan di **frame kunci**, untuk cek konsistensi identitas —
bukan setiap frame. Itulah sebabnya baris "per frame" ada, dan itu yang harus
mengikuti kamera.

**Input 320 adalah satu-satunya konfigurasi yang memenuhi A4** untuk frame biasa:
149,1 ms terhadap anggaran 150 ms. Tipis, dan di mesin yang bising. Default
diubah ke 320 karena itu.

## Terhadap kriteria A4

**A4 semula menuntut p95 < 150 ms per frame untuk pipeline penuh.** Setelah
diukur, kriteria itu menggabungkan dua hal yang biayanya berbeda 5×.

**Usulan revisi A4** — dipisah jadi dua, dengan angka yang benar-benar terukur:

| | Anggaran | Terukur (320) | Status |
|---|---|---|---|
| **A4a** Frame biasa (tanpa embedder) | p95 < 150 ms | **149,1 ms** | ✅ tipis |
| **A4b** Frame kunci (dengan embedder) | p95 < 900 ms | **796,4 ms** | ✅ |

Frame kunci muncul beberapa kali per sesi, bukan enam kali per detik, jadi
anggarannya memang beda ordo. Menuntut 150 ms untuk keduanya berarti menuntut
sesuatu yang tidak diperlukan dan tidak tercapai.

**Kalau A4a perlu lebih longgar:** ganti embedder ke `w600k_mbf.onnx` dari paket
`buffalo_s` (MobileFaceNet, bukan ResNet-50). Jauh lebih ringan, akurasi 1:N
turun. Belum diukur; relevan hanya kalau frame kunci jadi masalah.

Tiga hal yang masih belum diketahui:

1. ~~Berapa biaya tiga model sisanya.~~ ✅ Terukur di T13, tabel di atas.
2. **Seberapa besar noise VM ini melebih-lebihkan p95.** Perlu satu run di mesin
   yang tenang atau di Linux native. p50 ke p95 melebar 1,7×, yang tinggi.
3. **Berapa fps yang sebenarnya dibutuhkan demo.** SPEC mengasumsikan ~6 fps.
   Pada 149 ms per frame yang tercapai ~6,7 fps, jadi asumsinya pas — tapi kalau
   3 fps sudah cukup untuk challenge-response, anggarannya berlipat.

**Ditinjau ulang di T30** (kalibrasi) dan saat Checkpoint A dijalankan dengan
webcam sungguhan.

## Batas dari angka-angka ini

Adegan sintetis mengukur **throughput** dengan jujur — model mengerjakan jumlah
kerja yang sama apa pun isi gambarnya — tapi tidak mengatakan apa pun tentang
**kualitas deteksi**. Tidak ada wajah yang terdeteksi di keempat adegan itu.

Untuk angka yang berarti soal akurasi, arahkan `-images` ke korpus sungguhan.
Repo ini tidak memuatnya, dan itu disengaja: lihat SPEC §7 dan Open Question #3.
