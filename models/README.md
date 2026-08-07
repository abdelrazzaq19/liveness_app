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

| File | Peran | Ukuran |
|---|---|---|
| `det_10g.onnx` | Deteksi wajah (SCRFD-10GF) — bbox + 5 keypoint | 16,1 MB |
| `2d106det.onnx` | Landmark 106 titik | 4,8 MB |
| `w600k_r50.onnx` | Embedding wajah (ArcFace R50) — 512 dimensi | 166,3 MB |

Ketiganya datang dari satu paket, `buffalo_l.zip`. InsightFace tidak merilisnya
sebagai `.onnx` satuan, jadi `modelctl` mengunduh paketnya lalu mengangkat tiga
anggota yang dibutuhkan. Pencocokan anggota memakai nama dasar, jadi perubahan
struktur direktori di dalam arsip tidak merusak manifest.

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
