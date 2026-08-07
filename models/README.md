# Direktori model

Direktori ini menampung file `.onnx`. **Isinya tidak di-commit** — hanya file ini
dan `manifest.json` (dibuat di T4) yang dilacak git.

Alasannya dua:

1. **Ukuran.** Keempat model berjumlah sekitar 190 MB. Repo git bukan tempatnya.
2. **Lisensi.** Model pre-trained InsightFace (SCRFD, ArcFace/buffalo_l) dirilis
   untuk **penggunaan riset non-komersial**. Mendistribusikan ulang lewat repo ini
   akan menyeret lisensi itu ke dalam project Anda. Setiap model mencatat
   lisensinya sendiri di `manifest.json`.

## Cara mengisi

```bash
docker compose --profile setup run --rm modelctl download
```

Perintah di atas belum ada — `cmd/modelctl` dibuat di **T4**. Sampai saat itu
direktori ini boleh kosong: `LV_PIPELINE_MODE` defaultnya `stub`, jadi service
dan seluruh test-nya berjalan tanpa satu pun file model.

## Model yang akan diunduh

| Peran | File | Perkiraan |
|---|---|---|
| Deteksi wajah | `scrfd_10g_bnkps.onnx` | ~17 MB |
| Landmark 106 titik | `2d106det.onnx` | ~5 MB |
| Anti-spoof pasif | `minifasnet_v2.onnx` | ~2 MB |
| Embedding wajah | `w600k_r50.onnx` | ~166 MB |

Nama file di atas bisa diubah lewat `LV_MODEL_*` di `.env`.
