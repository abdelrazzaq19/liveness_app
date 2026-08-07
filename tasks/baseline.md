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

## Terhadap kriteria A4

**A4 menuntut p95 < 150 ms per frame untuk pipeline penuh.**

| Konfigurasi | p95 detektor | Sisa anggaran untuk 3 model lain |
|---|---|---|
| 500M @ 640 (default sekarang) | 256,6 ms | **sudah lewat 107 ms** ⛔ |
| 500M @ 320 | 115,3 ms | 35 ms — kemungkinan besar tetap tidak cukup |

**Default tetap 640.** Alasannya: lingkungan pengukuran ini tidak layak dijadikan
dasar mengorbankan kualitas deteksi, dan ganti ke 320 hanya satu env var
(`LV_DETECTOR_INPUT_SIZE`). Tapi angka di atas berarti **A4 kemungkinan besar
harus direvisi**, bukan dikejar.

Yang belum diketahui dan menentukan:

1. **Berapa biaya tiga model sisanya.** Landmarker (4,8 MB) dan anti-spoof (2 MB)
   ringan. Embedder (166 MB, ResNet-50) tidak, tapi ia hanya jalan di frame kunci
   untuk cek konsistensi identitas — bukan setiap frame. Diukur di T13.
2. **Seberapa besar noise VM ini melebih-lebihkan p95.** Perlu satu run di mesin
   yang tenang, atau di Linux native.
3. **Berapa fps yang sebenarnya dibutuhkan demo.** SPEC mengasumsikan ~6 fps.
   Kalau 3 fps sudah cukup untuk challenge-response, anggarannya berlipat.

**Ditinjau ulang di T13** (pipeline penuh terukur) dan **T30** (kalibrasi).

## Batas dari angka-angka ini

Adegan sintetis mengukur **throughput** dengan jujur — model mengerjakan jumlah
kerja yang sama apa pun isi gambarnya — tapi tidak mengatakan apa pun tentang
**kualitas deteksi**. Tidak ada wajah yang terdeteksi di keempat adegan itu.

Untuk angka yang berarti soal akurasi, arahkan `-images` ke korpus sungguhan.
Repo ini tidak memuatnya, dan itu disengaja: lihat SPEC §7 dan Open Question #3.
