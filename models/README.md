# Direktori model

Direktori ini menampung file `.onnx`. **Isinya tidak di-commit** — hanya file ini
dan `manifest.json` yang dilacak git.

Alasannya dua:

1. **Ukuran.** Paket sumbernya 275 MB, hasil ekstraksinya 187 MB. Repo git bukan
   tempatnya.
2. **Lisensi.** Model pre-trained InsightFace dirilis untuk **penggunaan riset
   non-komersial**. Mendistribusikan ulang lewat repo ini akan menyeret lisensi
   itu ke dalam project Anda. Lisensinya tercatat per artefak di `manifest.json`.

## Cara mengisi

```bash
docker compose --profile setup run --rm modelctl download
```

Memeriksa yang sudah ada tanpa menyentuh jaringan:

```bash
docker compose --profile setup run --rm modelctl verify
```

Kalau ada file rusak atau hilang, `verify` keluar dengan kode non-zero dan
menyebut file mana yang bermasalah — semuanya sekaligus, bukan satu per
menjalankan.

Selama direktori ini kosong, service tetap jalan: `LV_PIPELINE_MODE` defaultnya
`stub`, dan seluruh test suite lolos tanpa satu pun file model.

## Isi setelah download

| File | Peran | Paket | Ukuran |
|---|---|---|---|
| `det_500m.onnx` | **Deteksi wajah (SCRFD-500M)** — bbox + 5 keypoint | `buffalo_s` | 2,4 MB |
| `2d106det.onnx` | Landmark 106 titik | `buffalo_l` | 4,8 MB |
| `w600k_r50.onnx` | Embedding wajah (ArcFace R50) — 512 dimensi | `buffalo_l` | 166,3 MB |
| `det_10g.onnx` | SCRFD-10GF — **tidak dipakai**, ikut terbawa dari paketnya | `buffalo_l` | 16,1 MB |

InsightFace tidak merilis model ini sebagai `.onnx` satuan, jadi `modelctl`
mengunduh paketnya lalu mengangkat anggota yang dibutuhkan. Pencocokan anggota
memakai nama dasar, jadi perubahan struktur direktori di dalam arsip tidak
merusak manifest.

### Kenapa SCRFD-500M, bukan SCRFD-10GF

Terukur 60 sampel per konfigurasi pada CPU 8 core — metodologi dan peringatannya
di [tasks/baseline.md](../tasks/baseline.md):

| Model | 640 p50 | 640 p95 | 320 p50 | 320 p95 |
|---|---|---|---|---|
| **SCRFD-500M** | **131,9 ms** | 256,6 ms | 60,6 ms | 115,3 ms |
| SCRFD-10GF | 985,9 ms | 1269,5 ms | 305,8 ms | 477,2 ms |

Yang ringan di resolusi penuh (132 ms) mengalahkan yang berat di seperempat
resolusi (306 ms), jadi resolusi tidak perlu dikorbankan demi kecepatan.

`det_10g.onnx` tetap ada di disk karena ikut terbawa dalam paket `buffalo_l`, dan
berguna sebagai pembanding di benchmark.

## ⛔ Anti-spoof pasif belum terselesaikan

`LV_MODEL_ANTISPOOF` menunjuk ke `minifasnet_v2.onnx`, dan **file itu tidak akan
pernah muncul dari `modelctl download`.**

MiniFASNetV2 berasal dari [Silent-Face-Anti-Spoofing][sfas], yang merilis
checkpoint PyTorch `.pth` — bukan ONNX. Tidak ada URL yang bisa diunduh dan
di-verifikasi begitu saja, jadi `modelctl` tidak mencantumkannya di manifest
alih-alih berpura-pura bisa mengambilnya.

Ini **memblokir T11**, tidak memblokir T5–T10. Opsi dan rekomendasinya ada di
[SPEC.md](../SPEC.md) Open Question #9.

[sfas]: https://github.com/minivision-ai/Silent-Face-Anti-Spoofing

## Tentang digest di manifest

`manifest.json` bekerja seperti lockfile. SHA-256 di dalamnya **direkam dari
unduhan nyata** (perintah `modelctl pin`), bukan diterbitkan oleh upstream.

Artinya: digest ini **tidak** membuktikan modelnya tepercaya. Yang dibuktikannya
adalah tidak ada yang menukar file itu sejak di-pin, dan checkout baru akan
mendapat byte yang persis sama.

Merekam ulang digest — hanya perlu kalau upstream sengaja mengganti rilisnya:

```bash
docker compose --profile setup run --rm modelctl pin
```
