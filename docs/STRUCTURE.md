# Struktur project

Dibaca dari disk, bukan dari ingatan. Perbarui kalau ada paket baru.

```
liveness-verifier/
│
├── cmd/                            entrypoint — satu-satunya yang tahu semua paket ada
│   ├── server/
│   │   ├── main.go                   boot, peringatan, graceful shutdown
│   │   └── wire.go                   SATU-SATUNYA tempat dependency disambung
│   ├── modelctl/                     unduh, verifikasi digest, pin manifest
│   ├── bench/                        ukur latensi pipeline
│   └── calibrate/                    sapu ambang jadi titik kerja (T30)
│
├── internal/
│   │
│   ├── config/                     sumber tunggal ~60 tunable
│   │   └── config.go                 melaporkan SEMUA error sekaligus, bukan yang pertama
│   │
│   ├── biometric/                  ┌─ DOMAIN ─ tidak tahu ONNX ada
│   │   ├── types.go                │  tipe bersama
│   │   ├── ports.go                │  antarmuka: Detector, Landmarker, AntiSpoofer, Embedder
│   │   ├── metrics.go              │  peta indeks 106 landmark, EAR/MAR
│   │   ├── pose.go                 │  perspektif lemah — tanpa focal length kamera
│   │   ├── embedding.go            │  vektor 512-D, cosine, normalisasi
│   │   ├── pipeline.go             │  orkestrator + gerbang kualitas
│   │   │                           └─
│   │   ├── onnx/                   SATU-SATUNYA paket yang impor onnxruntime_go
│   │   │   ├── runtime.go            muat libonnxruntime.so
│   │   │   ├── pool.go               session pool — ort.Session TIDAK thread-safe
│   │   │   ├── detector_scrfd.go     deteksi wajah (SCRFD-500M)
│   │   │   ├── landmarker_2d106.go   106 titik
│   │   │   ├── antispoof_minifas.go  ⚠ penegakan DIMATIKAN — lihat catatan di bawah
│   │   │   └── embedder_arcface.go   ArcFace w600k_r50
│   │   │
│   │   └── stub/                   pipeline tiruan deterministik, tanpa file model
│   │       └── stub.go               keluarannya diturunkan dari piksel; ini yang
│   │                                 membuat suite serangan bisa jalan tanpa .onnx
│   │
│   ├── imaging/                    decode, gerbang kualitas, alignment, pHash
│   │   └── exif.go                   parser EXIF tulis tangan, tanpa dependensi
│   │
│   ├── liveness/                   ┌─ DOMAIN ─ tidak impor net/http
│   │   ├── session.go              │  state machine sebagai TABEL, bukan if bertebaran
│   │   ├── challenge.go            │  undian acak per sesi
│   │   ├── evaluator.go            │  kedip relatif terhadap mata subjek sendiri;
│   │   │                           │  tengok & angguk sebagai perpindahan, bukan sudut
│   │   ├── antireplay.go           │  enam lapis pertahanan
│   │   └── service.go              │  orkestrasi, pengulangan langkah, otorisasi nonce
│   │                               └─
│   ├── enrollment/                 ┌─ DOMAIN ─ tidak tahu liveness ada
│   │   ├── types.go                │  Face, Repository, validasi
│   │   ├── token.go                │  token sekali pakai, disimpan sebagai HMAC
│   │   ├── audit.go                │  jejak yang TIDAK BISA dipisah dari barisnya
│   │   ├── artifact.go             │  retensi gambar — mati secara default
│   │   └── service.go              │  pengikatan identitas ke capture terverifikasi
│   │                               └─
│   ├── calibrate/                  sapu FAR/FRR — tanpa gambar, tanpa model
│   │   └── sweep.go                  menolak memberi ambang yang menolak semua orang
│   │
│   ├── httpapi/                    ┌─ TRANSPORT ─ tanpa business logic
│   │   ├── router.go               │  TIGA grup auth yang berbeda
│   │   ├── liveness_handler.go     │  + penerbitan token saat lolos
│   │   ├── faces_handler.go        │  galeri
│   │   ├── dto.go errors.go        │  bentuk request/response, amplop error
│   │   ├── middleware.go           │  request id, log, recover, timeout, API key
│   │   ├── ratelimit.go            │  token bucket per IP; X-Forwarded-For tidak dipercaya
│   │   ├── readiness.go            │  /readyz — "bisa melayani", bukan "hidup"
│   │   └── web.go                  │  sematkan demo ke biner
│   │                               └─
│   └── storage/                    ADAPTER
│       ├── postgres/                 db, session_repo, face_repo, token_store
│       └── objectstore/              minio
│
├── migrations/                     6 migrasi, semuanya reversibel
│   ├── 00001_init_extensions.sql     pgvector
│   ├── 00002_liveness_sessions.sql
│   ├── 00003_session_retries.sql
│   ├── 00004_face_gallery.sql        + index HNSW
│   ├── 00005_liveness_tokens.sql
│   └── 00006_audit_log.sql
│
├── web/static/                     demo webcam — disematkan ke biner, tanpa build step
│
├── tests/
│   ├── attack/                       10 serangan; JALAN DI TEST BIASA, bukan di balik tag
│   └── integration/                  terhadap Postgres & MinIO sungguhan (tag: integration)
│
├── tools/antispoof/                konversi checkpoint PyTorch → ONNX
│   ├── convert.py                    memakai definisi model upstream, bukan reproduksi
│   └── probe.py                      pembanding: membuktikan jalur Go setia ke PyTorch
│
├── docs/
│   ├── API.md                        kontrak; tiap status code diverifikasi ke server hidup
│   └── STRUCTURE.md                  berkas ini
│
├── deploy/Dockerfile               multi-stage: ort → builder → runtime, plus stage dev
├── compose.yaml                    postgres + minio + api (+ dev, modelctl, antispoof)
├── .devcontainer/                  editor di dalam container yang punya cgo
│
├── SPEC.md                         spesifikasi hidup, termasuk Open Question yang terbuka
├── tasks/plan.md                   31 task, 6 fase, urutan berbasis risiko
├── tasks/todo.md                   status per task + bukti; termasuk yang GAGAL
└── tasks/baseline.md               latensi terukur
```

---

## Empat aturan dependensi, dan alasannya

Aturan tanpa alasan adalah aturan yang dilanggar orang berikutnya dengan niat baik.

1. **`liveness` dan `enrollment` tidak impor `net/http`.**
   Domain yang tahu tentang HTTP akan mulai mengambil keputusan berdasarkan
   status code.

2. **Hanya `biometric/onnx` yang impor `onnxruntime_go`.**
   Itu yang membuat pipeline stub mungkin — dan stub itu yang membuat suite
   serangan bisa jalan tanpa satu pun file model.

3. **`liveness` tidak tahu `enrollment` ada.**
   Ketergantungannya menunjuk ke dalam: `enrollment` mendeklarasikan antarmuka
   kecil untuk apa yang dibutuhkannya dari sesi, dan `cmd/server` yang
   menyambungkan. Adapternya tinggal di sana karena hanya di sanalah kedua
   paket boleh diketahui bersamaan.

4. **Wiring hanya di `cmd/server`.**
   Ia satu-satunya yang tahu setiap paket ada.

---

## Angka

| | |
|---|---|
| Kode non-test | 11.405 baris |
| Kode test | **12.661 baris** |
| Paket `internal` | 73 berkas |
| Migrasi | 6, semuanya naik-turun-naik terverifikasi |

Test lebih banyak daripada kodenya. Itu bukan kebetulan — dan tetap saja empat
bug yang membuat sistem ini tidak bisa dipakai satu orang pun lolos dari
semuanya: frame terlalu kecil, ambang kedip yang mustahil dipenuhi, anti-spoof
yang menolak 100% pengguna asli, dan model pose yang nyaris koplanar.

Yang menemukannya bukan test, melainkan seseorang mencoba memakainya.

---

## Catatan tentang ⚠ pada `antispoof_minifas.go`

Kodenya **benar**. Logits yang dihasilkan jalur Go identik dengan PyTorch
aslinya, dibuktikan oleh `tools/antispoof/probe.py`.

Modelnya yang memberi skor ~0,006 untuk wajah asli terhadap ambang 0,80 — ia
menolak setiap subjek sungguhan. Penegakannya dimatikan lewat
`LV_LIVENESS_ANTISPOOF_ENFORCE=false`, skornya tetap diukur dan dicatat, dan
server memperingatkan setiap boot.

Selama itu, **foto cetak dan replay layar tidak diblokir.**
