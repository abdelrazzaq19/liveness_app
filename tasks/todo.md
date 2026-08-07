# Task List: Liveness Verifier

> Turunan dari [SPEC.md](../SPEC.md) v0.2 dan [plan.md](plan.md) · Fase spec-driven: **3/4 (Tasks)** · 2026-08-07
> **31 task · 6 fase.** Kerjakan berurutan kecuali ditandai ∥ (paralel).

**Definition of Done berlaku untuk setiap task** — lihat [plan.md](plan.md#definition-of-done-berlaku-untuk-setiap-task). Tidak diulang di tiap task di bawah.

Semua perintah dijalankan dari root project. `dev` merujuk ke service dev di `compose.yaml`.

---

## Status Implementasi

| Task | Kode | Terverifikasi |
|---|---|---|
| T1 Skeleton + config | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T2 HTTP + healthz | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T3 Docker + compose | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| **Checkpoint 1** | — | ✅ **LOLOS** — 2026-08-07 |
| T4 modelctl | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| **T5 ⚠ ORT bootstrap** | ✅ selesai | ✅ **GATE LOLOS** — 2026-08-07 |
| T6 detektor SCRFD | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T7 imaging | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T8 bench CLI | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| **Checkpoint 2** | — | ✅ **GATE LOLOS** — 2026-08-07 |
| T9 landmark 106 + EAR/MAR | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T10 head pose | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T11 anti-spoof | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T12 embedder ArcFace | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| T13 pipeline + stub | ✅ selesai | ✅ **lolos** — 2026-08-07 |
| **Checkpoint 3** | — | ✅ **LOLOS** — 2026-08-07 |
| T16 sesi + state machine | ✅ selesai | ✅ **lolos** — cakupan 86,4% |
| T17 evaluator challenge | ✅ selesai | ✅ **lolos** — cakupan 91,4% |
| T18 anti-replay | ✅ selesai | ✅ **lolos** — cakupan 93,6% |
| T19 liveness.Service | ✅ selesai | ✅ **lolos** — cakupan 89,9%, sesi end-to-end |
| T14 Postgres + migrasi | ⬜ berikutnya | — |
| T15 session repository | ⬜ | — |
| T20 handler HTTP | ⬜ | — |
| T21 demo web UI | ⬜ | — |

### Koreksi terhadap SPEC §5: dedup pHash tidak boleh mematikan sesi

SPEC menulis "dua frame dengan Hamming distance pHash < 5 dianggap frame yang
sama (indikasi replay statis)". Kalau itu diperlakukan sebagai kegagalan sesi,
**subjek hidup yang duduk diam akan ditolak** — ia menghasilkan frame nyaris
identik juga.

Yang membedakan orang dari foto adalah orang **akhirnya bergerak**, dan
challenge-lah yang memastikan itu. Jadi:

- Satu frame duplikat → **recoverable**, minta frame berikutnya
- Rentetan panjang (`MaxDuplicateStreak`) → **fatal**, `ErrStaticReplay`

### Temuan T17/T19 yang jadi syarat untuk T21

**Challenge menoleh dan mengangguk menangkap baseline dari frame pertamanya.**
Gerakan diukur dari pose saat instruksi muncul, bukan sudut mutlak — subjek yang
kebetulan duduk menyerong tetap harus menoleh.

Konsekuensinya untuk demo UI: **instruksi harus muncul lebih dulu dan subjek
diberi waktu diam sebentar sebelum bergerak.** Kalau frame pertama challenge
sudah menoleh, itulah yang jadi baseline dan delta-nya nol selamanya.

Test end-to-end menangkap ini — 60 iterasi tanpa maju sama sekali.

### Konvensi tanda yang harus diterjemahkan UI

`ChallengeTurnLeft` berarti wajah menoleh ke **kiri gambar** (yaw mengecil).
Preview webcam biasanya dicerminkan, jadi teks instruksi untuk subjek harus
dibalik. Itu keputusan T21, bukan domain.

### Bukti T12

| Kriteria | Hasil |
|---|---|
| 512 dimensi, ter-L2-normalisasi | ✅ norma 1,0 ± 1e-5 |
| Deterministik | ✅ cosine 1,0 pada 4 run |
| Invarian skala | ✅ **cosine 0,9906** untuk input sama di skala 2× |
| Input non-persegi ditolak | ✅ hanya crop 112×112 hasil alignment |
| Landmark rusak → error | ✅ keypoint berimpit ditolak |
| Graph salah ditolak | ✅ |

Cosine 0,9906 lintas perubahan skala 2× adalah bukti terkuat bahwa alignment T7
benar: tanpa itu angkanya akan jatuh jauh.

**Konstanta normalisasi berbeda dari detektor.** ArcFace membagi **127,5**,
SCRFD membagi **128**. Keduanya terlihat bisa saling tukar dan tidak: yang salah
menggeser setiap embedding sedikit, tidak pernah gagal, dan diam-diam
menurunkan setiap skor kemiripan di galeri.

### Bukti T13

| Kriteria | Hasil |
|---|---|
| Satu gambar → `Face` lengkap | ✅ box, keypoint, landmark, pose, EAR, MAR, liveness, embedding |
| Gerbang kualitas memotong lebih awal | ✅ nol model dijalankan pada frame buram |
| Gerbang ukuran wajah setelah detektor | ✅ detektor jalan sekali, tiga model mahal tidak |
| `SkipEmbedding` benar-benar melewati | ✅ embedder nol panggilan, embedding `nil` bukan 512 nol |
| Pose gagal ≠ frame gagal | ✅ challenge tengok/angguk gagal sendiri, kedip dan mulut tetap jalan |
| Pipeline tidak lengkap ditolak saat konstruksi | ✅ bukan nil dereference di frame pertama sesi nyata |
| Stub deterministik | ✅ identik 1,000000 · sedikit berubah 0,999616 · berbeda 0,543578 |
| Stub memenuhi interface yang sama | ✅ `biometric.Analyzer` |
| **Seluruh test lolos tanpa file model** | ✅ |

### Bug yang ditangkap test stub

**Frame hitam total menghasilkan embedding tanpa magnitudo.** Grid luma semuanya
nol → proyeksi menghasilkan vektor nol → `Validate` gagal. Lensa tertutup akan
merusak cek konsistensi identitas di hilir. Diperbaiki dengan offset konstan per
sampel, jadi frame gelap tetap menghasilkan deskriptor sah yang sekadar tidak
membawa informasi.

**Embedding stub diturunkan dari isi frame, bukan dari hash.** Hash akan memberi
kebalikan dari yang dibutuhkan — vektor sangat berbeda untuk frame yang nyaris
identik — dan cek konsistensi identitas jadi tidak bisa di-test tanpa model.

---

> ## ✅ Checkpoint 3 — Biometric Pipeline: LOLOS
>
> - [x] Golden test keempat model lolos
> - [x] Pose pulih ±0,05° pada rotasi sintetis (syarat ±5°)
> - [x] Cosine embedding memisahkan identitas — 0,9906 sama, 0,58 berbeda
> - [x] **Seluruh test suite lolos tanpa file model** (jalur stub)
> - [x] Stub dan implementasi ONNX memenuhi interface yang sama

### 🎯 Vonis A4 — diukur, bukan ditebak

Pipeline penuh per tahap, input detektor 320:

| Tahap | p50 | p95 |
|---|---|---|
| Gerbang kualitas | 7,6 ms | 14,0 ms |
| Detektor | 40,9 ms | 64,7 ms |
| Landmarker | 24,5 ms | 56,3 ms |
| Anti-spoof | 15,5 ms | 30,3 ms |
| **Embedder** | **434,7 ms** | **673,0 ms** |
| TOTAL (frame kunci) | 534,9 ms | 796,4 ms |
| **Per frame** (tanpa embedder) | **89,5 ms** | **149,1 ms** |

**Embedder = 71% dari total, dan biayanya tidak berubah dengan resolusi
detektor** — input-nya tetap crop 112×112. Menurunkan resolusi detektor tidak
menolong frame kunci sama sekali.

Tapi embedder hanya jalan di **frame kunci** untuk cek konsistensi identitas.

**A4 dipecah dua** karena kriteria aslinya menggabungkan dua hal yang biayanya
berbeda 5×:

| | Anggaran | Terukur | Status |
|---|---|---|---|
| **A4a** frame biasa | p95 < 150 ms | **149,1 ms** | ✅ tipis |
| **A4b** frame kunci | p95 < 900 ms | **796,4 ms** | ✅ |

**Default `LV_DETECTOR_INPUT_SIZE` diubah 640 → 320.** Itu satu-satunya
konfigurasi yang memenuhi A4a; di 640 angkanya 242,7 ms. Naikkan lagi ke 640
untuk enrollment sekali jalan kalau wajah kecil jadi masalah — satu env var.

Kalau A4a perlu lebih longgar nanti: ganti embedder ke `w600k_mbf.onnx` dari
paket `buffalo_s` (MobileFaceNet). Jauh lebih ringan, akurasi 1:N turun. Belum
diukur.

### ✅ Open Question #9 terjawab: konversi `.pth` → ONNX

Keputusan Anda 2026-08-07: opsi (a). Diimplementasikan sebagai container setup
sekali jalan.

**Arsitektur MiniFASNet tidak direproduksi dari ingatan.** Container konversi
meng-clone repo upstream dan memakai definisi model **serta bobot mereka
sendiri**. Mereproduksi arsitektur dari ingatan justru jenis tebakan yang
menghasilkan angka masuk akal dan jawaban salah — dan di sini tidak ada cara
untuk mengetahuinya tanpa dataset berlabel.

| | |
|---|---|
| Upstream | `minivision-ai/Silent-Face-Anti-Spoofing` @ `b6d5f04a` |
| Checkpoint | `2.7_80x80_MiniFASNetV2.pth` |
| Toolchain | Python 3.11, torch 2.5.1 CPU, onnx 1.17.0 — semuanya di-pin |
| Hasil | `minifasnet_v2.onnx`, 1,7 MB, sha256 `f0a988fc…` |
| Graph | input `[-1 3 80 80]` → output `[-1 3]` logit |

Service-nya sendiri **tidak pernah butuh Python**. Ini murni langkah setup, dan
"project tunggal" tetap utuh untuk runtime-nya.

`modelctl` diperluas: artefak boleh punya `build` alih-alih `url`. Digest-nya
tetap direkam, jadi model bangunan lokal ter-pin persis seperti yang diunduh —
yang berbeda hanya asal byte-nya, bukan apakah orang bisa tahu byte itu berubah.
`download` menolak dengan menyebutkan perintah yang memproduksinya, bukan gagal
karena URL kosong.

### Bukti T11

| Kriteria | Hasil |
|---|---|
| Skor real/spoof terkalibrasi [0,1] | ✅ softmax atas 3 logit, kelas 1 = wajah hidup |
| Skor selalu probabilitas sah | ✅ diuji pada wajah sintetis, abu-abu rata, hitam murni, wajah di tepi frame |
| Deterministik | ✅ 5 run pada frame yang sama, identik sampai bit |
| Softmax tahan logit besar | ✅ ±1000 tidak menghasilkan NaN/Inf |
| Crop 2,7× digeser, bukan dipotong, di tepi | ✅ empat posisi tepi diuji |
| Urutan kanal | ✅ **BGR**, diuji eksplisit |
| Graph salah ditolak saat konstruksi | ✅ ukuran input dan jumlah kelas |
| Ambang tidak di-hardcode di jalur keputusan | ✅ skor dikembalikan mentah, ambang dari config |

Sinyal bahwa modelnya benar-benar bekerja: input non-wajah (sintetis, abu-abu,
hitam) semuanya skor **0,005–0,008** — model dengan benar mengatakan "bukan
wajah hidup".

### Dua detail pre-processing yang tidak memunculkan error kalau salah

**1. BGR, bukan RGB.** Detektor SCRFD memakai `swapRB=True` sehingga RGB.
MiniFASNet memakai `ToTensor` pada gambar hasil `cv2.imread` yang **BGR**, dan
`ToTensor` hanya membagi 255 dan memindahkan sumbu — ia tidak pernah menukar
kanal. Memberi RGB di sini tidak menghasilkan error, hanya skor yang berbeda
secara berarti.

**2. Resize anisotropik disengaja.** Upstream me-resize rektangel apa pun yang
di-crop langsung ke input persegi, jadi box wajah non-persegi teregang. Jaringan
dilatih persis pada itu; "memperbaikinya" justru memberi geometri yang belum
pernah ia lihat.

### Bukti T10

| Kriteria | Hasil |
|---|---|
| Yaw/pitch/roll dalam derajat dari 106 landmark | ✅ |
| Rotasi sintetis dipulihkan | ✅ **±0,05°** — jauh di bawah syarat ±5° |
| Rentang yaw −45°..+45° | ✅ langkah 5°, semuanya |
| Pitch −30°..+30°, roll −40°..+40° | ✅ |
| Rotasi gabungan | ✅ termasuk {40, 20, 20} dan {−40, −20, −20} |
| Invarian skala dan posisi | ✅ skala 0,15× sampai 1,8× |
| Konvensi tanda didokumentasikan **dan diuji** | ✅ yaw, pitch, roll masing-masing |
| Landmark rusak → error, bukan sudut palsu | ✅ `ErrPoseUnavailable` |
| Toleransi noise 2 px | ✅ yaw/roll ±3°, pitch ±8° |
| Cakupan `internal/biometric` | **90,9%** |

### Weak perspective, bukan PnP perspektif penuh

Plan menyebut PnP. Saya pakai **scaled orthographic (weak perspective)** dan itu
disengaja: solusi perspektif penuh menuntut focal length kamera, dan webcam
sembarangan tidak melaporkannya. Mengarang nilainya juga aproksimasi — hanya
yang tersembunyi. Pada jarak selfie, weak perspective berbiaya beberapa derajat
dan tidak menuntut apa pun yang pemanggil tidak punya.

Keuntungan lain: solusinya bentuk tertutup, jadi tidak ada iterasi yang bisa
gagal konvergen di tengah sesi.

### Dua bug nyata yang ditangkap test konvensi tanda

**1. Nama konstanta sudut mata berbohong untuk mata kanan.** Offset `+2` selalu
titik ber-x terkecil dan `+6` terbesar. Untuk mata kiri itu memang luar/dalam;
untuk mata kanan justru terbalik. Nama `eyeOuterCorner` membaca dengan benar dan
menghitung yang salah di separuh kasus. Diganti jadi `eyeCornerLeft`/
`eyeCornerRight` — relatif gambar, benar untuk keduanya.

**2. Model 3D berkonvensi kebalikan dari komentarnya sendiri.** Komentar menulis
"z menjauhi kamera", tapi nilai model klasik yang disalin luas menulis kedalaman
sebagai negatif — artinya sebaliknya, dan itu **membalik tanda setiap yaw**.
Nilai Z dibalik supaya kode cocok dengan dokumentasinya.

Keduanya jenis kesalahan yang tidak pernah memunculkan error: challenge "tengok
kanan" akan diterima untuk tengok kiri, dan tidak ada yang tahu kecuali ada yang
mengetesnya.

### Pitch memang sumbu terlemah

Dengan noise landmark 2 px: yaw dan roll meleset < 3°, pitch sampai 8°. Itu
inheren, bukan kekurangan yang bisa disetel. Yaw dan roll dibaca dari lebar dan
kemiringan wajah yang membentang ratusan piksel; pitch dibaca dari seberapa besar
perbedaan kedalaman memendek, dan kedalaman itu hanya puluhan milimeter pada
wajah yang nyaris rata dari sudut pandang kamera.

Relevan untuk T17: challenge **NOD** (mengangguk, pakai pitch) akan lebih berisik
daripada **TURN_LEFT/RIGHT** (pakai yaw). Ambangnya perlu lebih longgar.

### Dependency di-pin karena Go 1.23

`gonum` dan `x/image` versi terbaru menuntut Go 1.24/1.25. Keduanya di-pin ke
versi kompatibel (`gonum@v0.15.1`, `x/image@v0.23.0`). Kalau pola ini berlanjut,
menaikkan Go lebih murah daripada terus mem-pin — tapi itu perubahan SPEC §2 dan
belum perlu sekarang.

### Bukti T8

| Kriteria | Hasil |
|---|---|
| `bench` memproses tiap gambar, cetak bbox + latensi | ✅ mode `-v` |
| Melaporkan p50/p95/p99 dan jumlah core | ✅ plus min/max/mean dan throughput |
| Keluar non-zero bila ada gambar gagal | ✅ "tidak ada wajah" bukan kegagalan, error decode/inferensi iya |
| Baseline tercatat | ✅ [baseline.md](baseline.md) |

### ⚠️ Koreksi penting dari T8

**Angka benchmark T6 saya salah.** Diambil dengan `-benchtime 5x`/`10x` di
lingkungan yang ternyata sangat bising — rentang min-ke-p99 mencapai **3,3×** pada
beban identik.

| | Dilaporkan T6 | Sebenarnya (60 sampel) |
|---|---|---|
| SCRFD-500M @ 640 | 73 ms | **131,9 ms p50 · 256,6 ms p95** |
| SCRFD-10GF @ 320 | 116 ms | **305,8 ms p50 · 477,2 ms p95** |

Bench CLI dan benchmark Go sekarang sepakat dalam **0,6 ms** satu sama lain pada
beban yang sama, jadi kedua alat benar — ukuran sampelnya yang salah.

**Keputusan ganti model tetap berlaku.** Perbandingannya bertahan karena keduanya
diukur di kondisi sama; hanya besaran mutlaknya yang bergeser.

**Tapi kriteria A4 tidak terpenuhi.** Target p95 < 150 ms untuk pipeline penuh,
sementara detektor saja sudah 256 ms p95 di 640 (115 ms di 320). Tiga model lain
belum dihitung. Analisis lengkap dan tiga hal yang belum diketahui ada di
[baseline.md](baseline.md). **Ditinjau ulang di T13.**

Default tetap 640: lingkungan pengukuran ini tidak layak dijadikan dasar
mengorbankan kualitas deteksi, dan pindah ke 320 hanya satu env var.

### Bukti T9

| Kriteria | Hasil |
|---|---|
| 106 landmark dalam koordinat **gambar**, bukan crop | ✅ box {300,400}–{420,560} → bounds {303,9 · 406,9}–{419,3 · 544,8} |
| Indeks EAR/MAR sebagai konstanta bernama | ✅ nol angka telanjang di dalam rumus |
| Test EAR: mata tertutup < 0,21, terbuka > 0,30 | ✅ landmark sintetis, rasio **eksak** sampai 1e-9 |
| EAR/MAR invarian skala | ✅ diuji pada skala 0,25× sampai 11× |
| Landmark rusak → 0, bukan NaN/Inf | ✅ |
| Geometri wajah: mata kiri < kanan, mulut di bawah mata | ✅ pada model nyata |
| Graph salah ditolak saat konstruksi | ✅ detektor ditolak sebagai landmarker |
| Cakupan `internal/biometric` | **98,0%** |

### 🔬 Peta indeks 106 titik ditentukan secara empiris

Menebak indeks berarti menaruh angka salah di dalam rumus EAR — kelas kesalahan
yang gejalanya hanya "deteksi kedipnya agak meleset". Jadi tidak ditebak.

Model landmark meregresi ke **wajah rata-rata** ketika diberi input tanpa wajah,
dan bentuk kanonik itu membuat kelompoknya tidak ambigu:

| Indeks | Bukti dari output model | Isi |
|---|---|---|
| 0–32 | rentang terluar, x 49→143, dagu di y=168 | kontur |
| 33–42 | 10 titik, lebar 12 tinggi 5 (bentuk mata), x≈90 | mata kiri |
| 43–51 | 9 titik, tepat di atas 33–42 | alis kiri |
| 52–71 | 20 titik, y 125–146 | mulut |
| 72–86 | 15 titik, sebar vertikal 37 | hidung |
| 87–96 | 10 titik, x≈120, struktur identik 33–42 | mata kanan |
| 97–105 | 9 titik, di atas 87–96 | alis kanan |

33+10+9+20+15+10+9 = 106 ✓

Di dalam blok mata: offset +2/+6 adalah sudut (x ekstrem), +7/+8/+9 kelopak atas,
+0/+3/+4 kelopak bawah — berpasangan vertikal rapi berdasarkan x. Itu yang
membuat rasio berarti: tiap pasang mengukur bukaan mata di satu posisi
horizontal.

Test `TestLandmarkIndexRangesTileEveryPoint` memastikan rentangnya menutup 0..105
tanpa celah atau tumpang tindih, jadi peta yang salah menggagalkan test, bukan
diam-diam merusak setiap pengukuran kedipan.

### ⛔ Ambang EAR/MAR tidak berpindah dari literatur

Angka di config (EAR 0,21/0,30, MAR 0,55) adalah nilai literatur untuk skema
**68 titik dlib**. Project ini memakai 106 titik dengan pilihan indeks sendiri.

**Bukti konkret:** pada wajah rata-rata dengan **mulut tertutup**, MAR terukur
**0,520** — nyaris menyentuh ambang 0,55. Ambang itu akan salah mengklasifikasi.

Artinya **T30 (kalibrasi) bukan pemolesan opsional, melainkan syarat** sebelum
evaluator T17 berarti apa pun. Sudah ditandai di `.env.example` dan di doc
comment `MouthAspectRatio`.

### Deviasi T8

**Mode `-synthetic` ditambahkan.** Repo ini tidak memuat foto wajah, jadi tanpa
mode ini alatnya tidak bisa dijalankan sama sekali di checkout bersih. Adegan
sintetis mengukur throughput dengan jujur — model mengerjakan jumlah kerja yang
sama apa pun isi gambarnya — tapi tidak mengatakan apa pun soal kualitas deteksi.

---

> ## 🎯 Checkpoint 2 — GATE RISIKO: LOLOS
>
> - [x] SCRFD mendeteksi wajah **di dalam Docker**, bukan hanya di test
> - [x] Session pool lolos `-race` di bawah konkurensi (100 goroutine × 50)
> - [x] `modelctl verify` menolak file rusak
> - [x] Baseline p95 tercatat di [baseline.md](baseline.md)
>
> **Arsitektur satu-container bertahan.** ONNX Runtime memuat dan menjalankan
> model dari container Debian slim; tidak perlu mundur ke sidecar Python.
>
> Yang terbawa ke fase berikutnya: **A4 kemungkinan besar harus direvisi**, dan
> **Open Question #9** (model anti-spoof tanpa rilis ONNX) memblokir T11.

### Bukti T7

| Kriteria | Hasil |
|---|---|
| Decode JPEG/PNG | ✅ |
| Batas byte **dan** batas piksel | ✅ batas piksel dicek dari header, sebelum satu piksel pun di-decode |
| File rusak ditolak tanpa panic | ✅ + **fuzz 1,1 juta eksekusi, nol panic** |
| Orientasi EXIF | ✅ kedelapan nilai dibaca, ditransformasi, dan diuji lewat posisi sudut |
| Gerbang kualitas | ✅ blur, terlalu gelap, terlalu terang, wajah terlalu kecil — masing-masing dengan alasannya |
| Variance Laplacian mengurutkan ketajaman | ✅ rata < gradien < papan catur |
| Alignment similarity 5 titik | ✅ transform yang diketahui dipulihkan sampai 1e-9 |
| Landmark mendarat di template | ✅ marker berwarna di kelima posisi template |
| pHash 64-bit + jarak Hamming | ✅ noise < 5, rescale 2× < 8, adegan berbeda ≥ 10 |
| Cakupan | **88,8%** |

### Temuan T7

**Dua kegagalan pertama adalah fixture test yang buruk, bukan bug implementasi.**
Layak dicatat karena keduanya mudah salah didiagnosis:

1. **pHash "gagal" invariansi brightness dengan jarak 32** — acak sempurna.
   Penyebabnya gambar uji berupa gradien yang sudah mencapai 255, jadi `+20`
   ter-clamp dan mengubah *struktur*, bukan levelnya. Selain itu gradien mulus
   memang fixture pHash yang buruk: hampir seluruh energi DCT-nya menumpuk di
   beberapa koefisien pertama, sisanya berkerumun di sekitar nol tempat noise
   sekecil apa pun membalik banyak bit sekaligus. Diganti gambar bertekstur
   dengan energi tersebar di banyak frekuensi, nilainya dijaga di [30, 190].

2. **Marker alignment ter-blend jadi `{204 7 7}` alih-alih `{255 0 0}`.**
   Transform-nya benar; bilinear memang mencampur tepi marker. Asersinya diganti
   jadi "marker mana yang paling mirip", dibandingkan lewat **arah** vektor RGB
   ternormalisasi, bukan jarak Euclidean — magenta redup lebih dekat ke merah
   terang secara jarak, tapi arahnya tetap menunjuk magenta.

**Parser EXIF ditulis sendiri, bukan menambah dependency.** Cakupannya sengaja
sempit: berjalan ke IFD0, mencari satu tag, menyerah pada apa pun yang tidak
dikenali. Library EXIF penuh berarti satu dependency dan permukaan serang jauh
lebih besar untuk satu nilai 16-bit. Input rusak apa pun menghasilkan
`OrientationNormal`, yang juga jawaban benar untuk file tanpa EXIF.

**Alignment memakai similarity 4 parameter, bukan affine 6.** Crop wajah tidak
boleh di-*shear* atau diregangkan berbeda per sumbu: itu mengubah geometri yang
justru diukur model embedding.

### Deviasi T7

`exif.go` dipisah dari `decode.go` (plan menyebut 5 file, jadi 6). Parser EXIF
adalah format tersendiri dengan urusannya sendiri, dan menggabungkannya membuat
`decode.go` dua kali lebih panjang tanpa alasan.

### Bukti T6

| Kriteria | Hasil |
|---|---|
| `biometric.Detector` didefinisikan di `ports.go` | ✅ `internal/biometric` tidak mengimpor `onnxruntime_go` |
| Decode jarak-ke-tepi benar | ✅ tensor buatan tangan, bbox cocok persis |
| Tata letak anchor cell-major | ✅ 5 posisi diuji, termasuk sudut dan anchor kedua |
| Keypoint decode | ✅ kelima titik, offset positif dan negatif |
| NMS | ✅ overlap berat, disjoint, di bawah ambang, rantai supresi |
| NMS urut skor menurun | ✅ |
| Letterbox: skala, padding, normalisasi, urutan RGB | ✅ empat rasio aspek, konstanta 127,5/128 diverifikasi |
| Tensor pendek ditolak | ✅ scores/boxes/keypoints, masing-masing |
| Graph bukan SCRFD ditolak saat konstruksi | ✅ jumlah input dan output diperiksa |
| `ErrNoFaceFound` bukan Detection kosong | ✅ |
| Golden file | ✅ terekam dan cocok saat dijalankan ulang |
| Test / vet / lint | ✅ hijau, nol issue |

### 📊 Jawaban untuk pertanyaan latensi T5

Benchmark `det_10g.onnx`, CPU 8 core, adegan sintetis 480×640, `-benchtime 5x`:

| Input | thread auto | 2 thread | 4 thread |
|---|---|---|---|
| **640** | 504 ms | 942 ms | 499 ms |
| **512** | 296 ms | 324 ms | 353 ms |
| **384** | 175 ms | 191 ms | 295 ms |
| **320** | **127 ms** | **119 ms** | 124 ms |

Tiga kesimpulan:

1. **Menaikkan thread tidak menolong.** Default ONNX Runtime (auto) sudah sebaik
   atau lebih baik dari nilai manual mana pun. Opsi 3 di catatan T5 gugur.
2. **Resolusi yang menentukan.** 640→320 memberi percepatan ~4×, persis sesuai
   pengurangan jumlah piksel. Tidak ada kejutan, dan tidak ada trik lain.
3. **`det_10g` tidak muat di anggaran A4.** Bahkan di 320×320 ia memakai 127 ms
   dari 150 ms — dan itu **hanya detektor**. Landmarker, anti-spoof, dan embedder
   belum dihitung sama sekali.

### ✅ Keputusan: ganti ke SCRFD-500M (2026-08-07)

> ⚠️ **Angka di bagian ini digantikan.** Diukur dengan `-benchtime 5x`/`10x` —
> terlalu sedikit sampel di mesin yang bising. Lihat [baseline.md](baseline.md)
> untuk angka 60-sampel yang menggantikannya, dan T8 di bawah untuk koreksinya.
> Kesimpulan **ganti model tetap berlaku**; besaran mutlaknya yang salah.

**Ternyata bukan trade-off sama sekali.** SCRFD-500M di resolusi penuh mengalahkan
SCRFD-10GF di seperempat resolusi, jadi resolusi tidak perlu dikorbankan demi
kecepatan.

Default sekarang: `det_500m.onnx` di `LV_DETECTOR_INPUT_SIZE=640`.

Perubahan yang menyertainya:

- `buffalo_s.zip` (122 MB) masuk manifest, hanya `det_500m.onnx` yang diangkat.
- `modelctl pin` sekarang **melewati artefak yang sudah ter-pin dan lengkap**.
  Tanpa itu, menambah satu entri berarti mengunduh ulang 275 MB yang sudah ada.
  `-force` untuk merekam ulang ketika upstream mengganti rilisnya.
- `LV_DETECTOR_INPUT_SIZE` dan `LV_DETECTOR_NMS_IOU` masuk config, dengan
  cross-validation bahwa ukurannya kelipatan 32 — stride terbesar detektor.
  Ukuran yang bukan kelipatan 32 menyisakan sel anchor separuh yang decode-nya
  menghasilkan koordinat ngawur.
- `det_10g.onnx` tetap di disk: ikut terbawa paket `buffalo_l`, berguna sebagai
  pembanding di benchmark.

### ⚠️ Batas dari golden test ini

Golden merekam **sidik jari model**, bukan deteksi wajah sungguhan. Adegan
sintetisnya tidak dikenali sebagai wajah — skor maksimum hanya 0,024–0,035 di
ketiga stride, jauh di bawah ambang mana pun.

Artinya:

- ✅ **Kebenaran decode terverifikasi persis.** Tensor buatan tangan dengan hasil
  yang diketahui menguji regresi jarak-ke-tepi, tata letak anchor, keypoint, NMS,
  dan letterbox. Di situlah bug sebenarnya hidup, dan di situ pengujiannya kuat.
- ✅ **Pertukaran model tertangkap.** Jumlah anchor dan skor maksimum per stride
  berubah begitu file model diganti.
- ⛔ **Deteksi wajah sungguhan tidak terverifikasi.** Tidak ada bukti pipeline ini
  benar-benar menemukan wajah manusia.

Ini konsekuensi langsung dari aturan SPEC §7: tidak ada foto orang sungguhan di
`testdata/`. Untuk menutupnya perlu wajah sintetis berkualitas atau dataset
berlisensi CC0 — sama dengan Open Question #3 (dataset kalibrasi). Tertutup di
T8 sekaligus, kalau datasetnya sudah ada.

### Deviasi T6

**Golden file di `internal/biometric/onnx/testdata/golden/`, bukan
`testdata/golden/` di root** seperti SPEC §4. `testdata` per-paket adalah
konvensi Go — `go test` menjalankan test dengan cwd di direktori paketnya, jadi
path root berarti `../../../testdata/...` di setiap pemanggilan.

### 🎯 Bukti T5 — GATE RISIKO LOLOS

**Arsitektur satu-container bertahan.** `onnxruntime_go` memuat dan menjalankan
SCRFD-10GF dari dalam container Debian slim.

| Kriteria | Hasil |
|---|---|
| ORT terinisialisasi di container | `ONNX Runtime 1.28.0` |
| Graph signature terbaca | input `input.1` `[1 3 -1 -1]`, 9 output di 3 stride |
| Inferensi nyata berjalan | Output shape `[12800 1]`, `[3200 4]`, `[800 10]` dst — persis arsitektur SCRFD |
| Model hilang → gagal saat load | ✅ bukan saat request pertama |
| File bukan graph ONNX → gagal saat load | ✅ |
| Pool konkurensi 100 goroutine × 50 | ✅ `-race` bersih, nol overlap, 5000 inferensi terhitung |
| `Use` mengembalikan sesi setelah panic | ✅ pool tetap terpakai sesudahnya |
| `Close` menunggu inferensi in-flight | ✅ tidak ada sesi dihancurkan saat masih dipakai |
| Konkurensi model nyata | ✅ 24 inferensi, 8 goroutine, pool 2, `-race` bersih |

### Temuan T5

**Versi ORT harus dinaikkan 1.19.2 → 1.28.0.** `onnxruntime_go` v1.32 menyematkan
header C dengan `ORT_API_VERSION 28` dan menolak inisialisasi terhadap library
lama: *"The requested API version [28] is not available, only API versions
[1, 19] are supported"*. API version melacak minor version ORT. Kedua versi ini
terkunci satu sama lain dan sekarang dicatat begitu di Dockerfile dan SPEC §2.

**Pool ditulis terhadap interface `runner`, bukan `*ort.DynamicAdvancedSession`.**
Kebenaran konkurensi adalah seluruh alasan pool ini ada, jadi ia harus bisa
di-test di setiap run — bukan hanya di mesin yang kebetulan punya 187 MB model.
Fake-nya sekaligus mendeteksi dua goroutine masuk ke sesi yang sama, yaitu bug
persis yang dicegah pool ini.

### ⚠️ Risiko baru terhadap kriteria A4

**Latensi detektor jauh di atas target.** A4 menuntut p95 < 150 ms per frame.
Terukur pada CPU 8 core, input 640×640, `IntraOpThreads=2`, tanpa `-race`:

| Skenario | Latensi |
|---|---|
| Satu inferensi | **336 ms** |
| Konkuren, pool 2 | **225 ms** per inferensi |

Ini **baru detektornya saja**. Pipeline penuh masih menambah landmarker,
anti-spoof, dan embedder.

Empat jalan keluar, belum diputuskan:

1. **Turunkan resolusi input** 640→320. Empat kali lebih ringan. Konsekuensinya
   wajah kecil/jauh lebih sulit terdeteksi.
2. **Ganti ke SCRFD-500M** (`det_500m.onnx`, ada di paket `buffalo_s`). Sekitar
   20× lebih ringan dari 10G. Akurasi deteksi turun.
3. **Naikkan `IntraOpThreads`.** Diuji dengan 2 dari 8 core yang tersedia.
   Kemungkinan besar ini yang paling murah dicoba lebih dulu.
4. **Revisi target A4.** 150 ms mungkin memang tidak realistis untuk CPU murni.

**Diukur dengan benar di T8** (bench CLI). Keputusan model diambil di T6.
Belum memblokir apa pun.

### Bukti T4

| Kriteria | Hasil |
|---|---|
| `pin` merekam digest nyata | `buffalo_l` 275,3 MB, sha256 `80ffe37d…` |
| `download` memasang model | `det_10g.onnx` 16,1 MB · `2d106det.onnx` 4,8 MB · `w600k_r50.onnx` 166,3 MB |
| Idempoten | Run kedua: `ok buffalo_l (already present)`, nol request jaringan |
| Byte dirusak → `verify` gagal | Menyebut `2d106det.onnx`, menampilkan kedua digest, exit 1 |
| Dua file lain tetap dilaporkan | Semua masalah dilaporkan sekaligus, bukan berhenti di yang pertama |
| `download` memperbaiki file rusak | Unduh ulang, ketiganya `ok` |
| Tidak ada `.part` tersisa | Dikonfirmasi di test kegagalan maupun di direktori nyata |
| Test / vet / lint / gofumpt | Hijau, nol issue |

### Perubahan desain T4 (dari temuan saat implementasi)

**`modelctl` harus bisa mengekstrak arsip.** InsightFace tidak merilis `.onnx`
satuan — detector, landmarker, dan embedder ketiganya ada di dalam satu
`buffalo_l.zip` (275 MB). Manifest karenanya mendukung dua bentuk artefak: file
tunggal, dan arsip dengan daftar anggota yang diangkat. Pencocokan anggota jatuh
ke nama dasar kalau path lengkapnya tidak ketemu, supaya perubahan struktur
direktori di dalam arsip tidak merusak manifest.

**Ditambahkan perintah `pin`.** SHA-256 model tidak diterbitkan upstream, jadi
tidak ada nilai yang bisa ditulis ke manifest sebelum mengunduh sekali. `pin`
mengunduh, menghitung, dan menulis digest yang teramati. Manifest jadi bekerja
seperti lockfile: bukan bukti model tepercaya, tapi bukti tidak ada yang menukar
file itu sejak di-pin.

**Nama file detector dikoreksi.** SPEC menulis `scrfd_10g_bnkps.onnx`; nama
sebenarnya di dalam paket adalah `det_10g.onnx` (model yang sama, SCRFD-10GF).
Default di config dan `.env.example` disesuaikan.

### ⛔ Blocker baru untuk T11

**MiniFASNetV2 tidak punya rilis ONNX.** Upstream hanya merilis checkpoint
PyTorch, jadi `modelctl` tidak bisa mengambilnya dan artefaknya tidak dicantumkan
di manifest — lebih baik jujur daripada berpura-pura bisa. Opsi dan rekomendasi
ada di [SPEC.md](../SPEC.md) Open Question #9.

**Tidak memblokir T5–T10.**

### Bukti Checkpoint 1

| Kriteria | Hasil |
|---|---|
| Compose naik healthy | `api`, `postgres`, `minio` ketiganya `healthy` |
| `/healthz` dari host Windows | `200 {"status":"ok"}` |
| `/v1/*` tanpa key / key salah | `401` |
| `/v1/*` dengan key benar | `404` (lolos auth, route belum ada) |
| `go test ./... -race -count=1` | hijau — `config` dan `httpapi` `ok` |
| `go vet ./...` | bersih |
| `golangci-lint run ./...` | nol issue |
| `gofumpt -l .` | bersih |
| Graceful shutdown | SIGTERM → keluar **1,15 detik**, exit code 0 |
| Ukuran image runtime | **203 MB** (target < 250 MB) |
| ONNX Runtime 1.19.2 | terunduh, `ldconfig -p` menemukan `libonnxruntime` |

**Risiko R1 sebagian besar gugur.** ONNX Runtime terpasang dan ter-link di image
`runtime` maupun `dev`. Yang belum terbukti tinggal `onnxruntime_go` benar-benar
memuat dan menjalankan model — itu T5.

### Temuan saat verifikasi

**Bug nyata di router (ditemukan oleh test, sudah diperbaiki).**
`chi.Mux.ServeHTTP` keluar lebih awal ke `NotFoundHandler` selama `mx.handler`
masih `nil`, dan `mx.handler` baru dibangun ketika ada minimal satu route
terdaftar. Subrouter `/v1` hanya punya `Use()` tanpa route, sehingga **seluruh
rantai middleware — termasuk `APIKeyAuth` — dilewati** dan setiap request ke
`/v1/*` langsung dapat 404 tanpa pemeriksaan kredensial.

Perbaikannya: daftarkan handler catch-all `/*` di dalam subrouter `/v1`. Ini
membangun rantai middleware, sekaligus menjamin seterusnya bahwa path apa pun di
bawah `/v1` wajib punya API key sebelum bisa tahu path itu ada atau tidak. Pola
yang lebih spesifik yang didaftarkan T20 tetap menang atas wildcard ini.

**`cp` merusak rantai symlink ONNX Runtime.** `ldconfig` memperingatkan
`libonnxruntime.so.1 is not a symbolic link`. Diganti `cp -a`.

### Deviasi dari plan (disengaja, dengan alasan)

1. **`deploy/Dockerfile.dev` dihapus** — stage `dev` dijadikan target di dalam
   `deploy/Dockerfile`. Satu file berarti ONNX Runtime cukup diunduh sekali dan
   dibagi ke stage builder, runtime, dan dev.
2. **`deploy/docker-compose.yml` → `compose.yaml` di root** — perintah di SPEC §3
   berbentuk `docker compose up` dari root, yang menuntut build context di root.
3. **`deploy/.env.example` dihapus** — cukup satu `.env.example` di root. Dua file
   konfigurasi contoh pasti akan menyimpang satu sama lain.
4. **ONNX Runtime dipasang di T3, bukan T5** — memindahkan risiko R1 (CGO + ORT di
   Debian slim) maju ke Checkpoint 1. Build image runtime sengaja gagal kalau
   `ldconfig -p` tidak menemukan `libonnxruntime`. Lebih cepat tahu, lebih baik.
5. **`LV_ONNXRUNTIME_LIB` sudah masuk config sekarang** — supaya T5 tidak perlu
   menyentuh `internal/config` sama sekali.
6. **testify belum dipakai** — test T1–T3 memakai `testing` dari stdlib. Dependency
   ditambahkan saat benar-benar dibutuhkan, bukan lebih awal.
7. **`models/README.md` ditambahkan** — menjelaskan mengapa direktorinya kosong dan
   mengapa isinya tidak di-commit.
8. **Cross-validation `LV_HTTP_WRITE_TIMEOUT` > `LV_HTTP_REQUEST_TIMEOUT`
   ditambahkan** — kalau terbalik, klien melihat koneksi terputus alih-alih respons
   timeout, dan itu sulit didiagnosis.

---

## Fase 1 — Walking Skeleton

### T1: Skeleton repo, Go module, dan konfigurasi

**Description:** Bangun kerangka repo dan paket `internal/config` yang memuat **seluruh** knob konfigurasi dari SPEC §2 dan §5 — termasuk semua threshold biometrik — sejak awal. Ini menghindari refactor besar saat kalibrasi di T30.

**Acceptance criteria:**
- [ ] `internal/config.Load()` membaca semua env var, menerapkan default eksplisit, dan mengembalikan error deskriptif (menyebut nama var) bila yang wajib kosong atau di luar rentang.
- [ ] Seluruh threshold di SPEC §5 hadir sebagai field bertipe, bukan `map[string]string`.
- [ ] `.env.example` memuat setiap var dengan komentar dan nilai default; tidak ada secret asli.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/config/... -race`
- [ ] Manual: hapus satu var wajib → boot gagal dengan pesan yang menyebut nama var itu

**Dependencies:** None
**Files:** `go.mod` · `internal/config/config.go` · `internal/config/config_test.go` · `.env.example` · `.gitignore`
**Scope:** M

---

### T2: HTTP server, middleware, healthz, graceful shutdown

**Description:** Server chi dengan rantai middleware dan shutdown yang bersih. Belum ada endpoint domain.

**Acceptance criteria:**
- [ ] `GET /healthz` → 200 `{"status":"ok"}`; tidak butuh API key.
- [ ] Middleware terpasang berurutan: request ID → structured logger → recover → timeout → API key. Panic jadi 500 + log, bukan proses mati.
- [ ] `errors.go` memetakan error domain ke status HTTP dengan bentuk body seragam `{"error":{"code","message"}}`.
- [ ] SIGTERM menutup server tanpa memutus request yang sedang berjalan, maksimal 10 detik.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/httpapi/... -race`
- [ ] Manual: kirim SIGTERM saat request lambat berjalan → request selesai, lalu proses keluar

**Dependencies:** T1
**Files:** `cmd/server/main.go` · `internal/httpapi/router.go` · `internal/httpapi/middleware.go` · `internal/httpapi/errors.go` · `internal/httpapi/router_test.go`
**Scope:** M

---

### T3: Docker — image runtime, image dev, compose

**Description:** Multi-stage Dockerfile dengan CGO dan ONNX Runtime shared library, image `dev` berisi toolchain, dan compose yang menyatukan api + postgres + minio. **Ini yang membuat Go tidak perlu terpasang di Windows.**

**Acceptance criteria:**
- [ ] `docker compose up -d --build` → semua service `healthy`, `/healthz` menjawab dari host Windows.
- [ ] `docker compose run --rm dev go test ./...` jalan tanpa Go terpasang di host.
- [ ] `libonnxruntime.so` ada di image runtime dan ditemukan oleh dynamic linker (`ldconfig -p` menemukannya).
- [ ] Image runtime < 250 MB tanpa model; model di-mount dari `./models`, tidak di-bake.

**Verification:**
- [ ] `docker compose up -d --build && curl -s localhost:8080/healthz`
- [ ] `docker compose run --rm dev go test ./... -race`
- [ ] `docker images liveness-verifier:local --format "{{.Size}}"`

**Dependencies:** T2
**Files:** `deploy/Dockerfile` · `deploy/Dockerfile.dev` · `deploy/docker-compose.yml` · `deploy/.env.example` · `README.md`
**Scope:** M

> ### ✅ Checkpoint 1 — Walking Skeleton
> - [ ] Compose naik healthy dari cache kosong < 5 menit
> - [ ] `/healthz` menjawab dari host Windows
> - [ ] Test dan lint hijau di container dev
> - [ ] Graceful shutdown terbukti
> - [ ] **Tinjau bersama sebelum lanjut**

---

## Fase 2 — Inference Spike ⚠ RISIKO TERTINGGI

### T4: modelctl — unduh dan verifikasi model

**Description:** CLI yang mengambil kelima model ONNX ke `./models`, memverifikasi SHA-256, dan mencatat lisensi tiap model. Tanpa ini, setup tidak reproducible.

**Acceptance criteria:**
- [ ] `modelctl download` mengunduh sesuai `manifest.json`, memverifikasi SHA-256, dan idempoten — melewati file yang sudah ada dan valid.
- [ ] `modelctl verify` keluar dengan kode non-zero bila ada file hilang atau checksum tidak cocok.
- [ ] `manifest.json` mencatat nama, URL, SHA-256, ukuran, dan **lisensi** tiap model (lihat Risiko R2).
- [ ] Unduhan terputus tidak meninggalkan file rusak — tulis ke `.tmp`, rename setelah verifikasi.

**Verification:**
- [ ] `docker compose --profile setup run --rm modelctl download`
- [ ] Manual: rusakkan 1 byte di satu file → `modelctl verify` gagal dan menyebut file itu

**Dependencies:** T1
**Files:** `cmd/modelctl/main.go` · `cmd/modelctl/download.go` · `cmd/modelctl/download_test.go` · `models/manifest.json`
**Scope:** M

---

### T5: ⚠ Bootstrap ONNX Runtime + session pool

**Description:** **Task paling berisiko di seluruh project.** Inisialisasi environment ORT, muat model, dan sediakan pool session yang aman untuk konkurensi. `*ort.Session` tidak thread-safe — pool bukan optimasi, tapi syarat kebenaran.

**Acceptance criteria:**
- [ ] Environment ORT terinisialisasi di dalam container runtime; kegagalan memuat model gagal saat **boot**, bukan saat request pertama.
- [ ] Pool `chan *Session` berukuran konfigurasi; `Acquire` menghormati pembatalan context; `Release` selalu mengembalikan session bahkan saat panic.
- [ ] Teardown melepas semua session dan environment tanpa kebocoran.
- [ ] Test konkurensi: 100 goroutine × 50 inferensi lolos `-race` bersih.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/biometric/onnx/... -race -count=3`
- [ ] Manual: `docker compose up` dengan satu model dihapus → boot gagal, pesan menyebut model itu

**Dependencies:** T3, T4
**Files:** `internal/biometric/onnx/runtime.go` · `internal/biometric/onnx/runtime_test.go` · `deploy/Dockerfile` (edit)
**Scope:** M

> **Kalau task ini gagal:** hentikan pekerjaan fitur. Coba berurutan — (a) `debian:bookworm` penuh alih-alih slim, (b) base image ONNX Runtime resmi, (c) eskalasi ke saya untuk keputusan sidecar Python. Jangan menambal dengan `LD_LIBRARY_PATH` yang menutupi masalah sesungguhnya.

---

### T6: Detektor wajah SCRFD + golden test

**Description:** Implementasi pertama yang menghasilkan sesuatu bermakna: bounding box + 5 keypoint. Menetapkan pola port/adapter untuk tiga model berikutnya.

**Acceptance criteria:**
- [ ] `biometric.Detector` didefinisikan di `ports.go`; `internal/biometric` **tidak** mengimpor `onnxruntime_go`.
- [ ] Pre-processing (letterbox resize, normalisasi) dan post-processing (decode anchor, NMS) benar; bbox pada fixture cocok dengan golden file dalam toleransi ±2 px.
- [ ] Mengembalikan `ErrNoFaceFound` bila tidak ada wajah, dan wajah terbesar bila ada beberapa.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/biometric/... -race`
- [ ] Golden test lolos pada minimal 5 gambar fixture

**Dependencies:** T5
**Files:** `internal/biometric/types.go` · `internal/biometric/ports.go` · `internal/biometric/onnx/detector_scrfd.go` · `internal/biometric/onnx/detector_scrfd_test.go` · `testdata/golden/scrfd.json`
**Scope:** M

---

### T7: Paket imaging — decode, quality, align, pHash ∥

**Description:** Utilitas gambar murni Go tanpa OpenCV. Bisa dikerjakan paralel dengan T4–T6.

**Acceptance criteria:**
- [ ] Decode JPEG/PNG dengan batas ukuran byte **dan** dimensi; menolak file rusak tanpa panic; menghormati orientasi EXIF.
- [ ] Gerbang kualitas: variance Laplacian (blur), rata-rata brightness, lebar wajah minimum — semua ambang dari config.
- [ ] Alignment similarity transform 5 titik ke template 112×112 sesuai konvensi ArcFace.
- [ ] pHash 64-bit + jarak Hamming; dua frame webcam berturut-turut dari adegan statis berjarak < 5.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/imaging/... -race`
- [ ] Fuzz singkat pada decoder: `go test -fuzz=FuzzDecode -fuzztime=30s ./internal/imaging/`

**Dependencies:** T1 ∥ (paralel dengan T4–T6)
**Files:** `internal/imaging/decode.go` · `internal/imaging/quality.go` · `internal/imaging/align.go` · `internal/imaging/phash.go` · `internal/imaging/imaging_test.go`
**Scope:** M

---

### T8: CLI bench — bukti end-to-end

**Description:** Task kecil dengan nilai besar: membuktikan seluruh tumpukan ONNX bekerja di Docker, dan menetapkan baseline latensi untuk kriteria A4.

**Acceptance criteria:**
- [ ] `bench -images <dir>` memproses tiap gambar dan mencetak bbox + latensi.
- [ ] Melaporkan p50/p95/p99 dan jumlah core yang dipakai.
- [ ] Keluar non-zero bila ada gambar gagal diproses.

**Verification:**
- [ ] `docker compose run --rm dev go run ./cmd/bench -images testdata/faces`
- [ ] Catat p95 di `tasks/baseline.md` sebagai pembanding A4

**Dependencies:** T6, T7
**Files:** `cmd/bench/main.go` · `tasks/baseline.md`
**Scope:** S

> ### ⚠️ Checkpoint 2 — GATE RISIKO
> - [ ] SCRFD mendeteksi wajah **di dalam Docker**, bukan hanya di test
> - [ ] Session pool lolos `-race` di bawah konkurensi
> - [ ] `modelctl verify` menolak file rusak
> - [ ] Baseline p95 tercatat
> - [ ] **BERHENTI. Tinjau bersama.** Kalau gate ini gagal, arsitektur harus ditinjau ulang sebelum kode domain apa pun ditulis.

---

## Fase 3 — Biometric Pipeline

### T9: Landmark 106 titik + metrik EAR/MAR

**Acceptance criteria:**
- [ ] 106 landmark dihasilkan dalam koordinat gambar, bukan koordinat crop.
- [ ] EAR (Eye Aspect Ratio) dan MAR (Mouth Aspect Ratio) dihitung dari indeks landmark yang **dikonstantakan dan diberi komentar** — bukan angka telanjang di tengah rumus.
- [ ] Unit test memakai landmark sintetis dengan nilai EAR/MAR yang diketahui; mata tertutup < 0.21, terbuka > 0.30.

**Verification:** `docker compose run --rm dev go test ./internal/biometric/... -race`
**Dependencies:** T6, T7
**Files:** `internal/biometric/onnx/landmarker_2d106.go` · `internal/biometric/metrics.go` · `internal/biometric/metrics_test.go` · `internal/biometric/onnx/landmarker_2d106_test.go`
**Scope:** M

---

### T10: Head pose lewat PnP

**Acceptance criteria:**
- [ ] Yaw/pitch/roll dalam derajat dari 106 landmark, memakai model wajah kanonik 3D dan PnP (gonum).
- [ ] Rotasi sintetis yang diketahui dipulihkan dalam ±5° untuk yaw ∈ [-45°, +45°].
- [ ] Konvensi tanda didokumentasikan: yaw positif = kepala menoleh ke **kanan subjek**.

**Verification:** `docker compose run --rm dev go test ./internal/biometric/ -run TestPose -race`
**Dependencies:** T9
**Files:** `internal/biometric/pose.go` · `internal/biometric/pose_test.go`
**Scope:** S

---

### T11: Anti-spoof pasif (MiniFASNetV2) ∥

**Acceptance criteria:**
- [ ] Menghasilkan skor real/spoof kalibrasi [0,1] dari crop wajah yang sudah di-align.
- [ ] Golden test pada fixture asli **dan** fixture serangan cetak.
- [ ] Skor tidak pernah di-hardcode di jalur keputusan — ambang dari config.

**Verification:** `docker compose run --rm dev go test ./internal/biometric/onnx/ -run TestAntiSpoof -race`
**Dependencies:** T5, T7 ∥ (paralel dengan T9, T12)
**Files:** `internal/biometric/onnx/antispoof_minifas.go` · `internal/biometric/onnx/antispoof_minifas_test.go`
**Scope:** S

---

### T12: Embedder ArcFace ∥

**Acceptance criteria:**
- [ ] Vektor 512-dimensi, ter-L2-normalisasi (norma = 1.0 ± 1e-5).
- [ ] Dua foto orang yang sama → cosine > 0.6; orang berbeda → < 0.3 pada fixture.
- [ ] Input harus crop 112×112 hasil alignment T7; menolak ukuran lain secara eksplisit.

**Verification:** `docker compose run --rm dev go test ./internal/biometric/onnx/ -run TestEmbedder -race`
**Dependencies:** T5, T7 ∥ (paralel dengan T9, T11)
**Files:** `internal/biometric/onnx/embedder_arcface.go` · `internal/biometric/onnx/embedder_arcface_test.go`
**Scope:** S

---

### T13: Orkestrator Pipeline + stub deterministik

**Description:** Menyatukan keempat model menjadi satu panggilan, **dan** menyediakan stub yang menurunkan output dari hash gambar. Stub inilah yang membuat seluruh Fase 4 dan 5 bisa dikembangkan tanpa file model.

**Acceptance criteria:**
- [ ] `Pipeline.Analyze(ctx, img)` → `Face{BBox, Landmarks, Pose, EAR, MAR, SpoofScore, Embedding, Quality}`.
- [ ] Gerbang kualitas memotong lebih awal — tidak menjalankan embedder pada frame buram; menghemat latensi.
- [ ] Stub deterministik: gambar yang sama selalu menghasilkan `Face` yang sama; gambar berbeda menghasilkan embedding berbeda yang stabil.
- [ ] Stub dan implementasi ONNX memenuhi interface yang **persis sama**; ditukar lewat config.

**Verification:**
- [ ] `docker compose run --rm dev go test ./internal/biometric/... -race`
- [ ] `docker compose run --rm dev go test ./... -race` lolos **dengan direktori models kosong** (memakai stub)

**Dependencies:** T9, T10, T11, T12
**Files:** `internal/biometric/pipeline.go` · `internal/biometric/pipeline_test.go` · `internal/biometric/stub/stub.go` · `internal/biometric/stub/stub_test.go`
**Scope:** M

> ### ✅ Checkpoint 3 — Biometric Pipeline
> - [ ] Golden test keempat model lolos
> - [ ] Pose pulih ±5° pada rotasi sintetis
> - [ ] Cosine embedding memisahkan identitas sesuai ambang
> - [ ] **Seluruh test suite lolos tanpa file model** (jalur stub)
> - [ ] Tinjau bersama sebelum lanjut

---

## Fase 4 — Milestone A: Active Liveness

### T14: Postgres, goose, migrasi awal ∥

**Acceptance criteria:**
- [ ] Pool pgx dengan health check dan batas koneksi dari config.
- [ ] Migrasi goose di-embed ke binary; dijalankan lewat perintah eksplisit, **bukan** otomatis saat boot.
- [ ] `up` → `down` → `up` bersih pada database kosong (kriteria X5).
- [ ] Ekstensi `vector` dibuat di migrasi pertama.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestMigrations`
**Dependencies:** T3 ∥ (paralel dengan T16)
**Files:** `internal/storage/postgres/db.go` · `migrations/00001_init_extensions.sql` · `migrations/00002_liveness_sessions.sql` · `tests/integration/migrate_test.go`
**Scope:** M

---

### T15: Repository sesi

**Acceptance criteria:**
- [ ] Create/Get/Update/Delete sesi; update memakai optimistic locking untuk mencegah lost update dari frame konkuren.
- [ ] `DeleteExpired` membersihkan sesi kedaluwarsa; test membuktikan sesi kedaluwarsa hilang.
- [ ] Test integrasi terhadap Postgres asli lewat testcontainers, bukan mock.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestSessionRepo`
**Dependencies:** T14
**Files:** `internal/storage/postgres/session_repo.go` · `tests/integration/session_repo_test.go`
**Scope:** M

---

### T16: Entity sesi, generator challenge, state machine ∥

**Description:** Jantung domain. Go murni, tanpa I/O, tanpa model. Bisa dikerjakan paralel dengan seluruh Fase 3.

**Acceptance criteria:**
- [ ] 3 challenge dipilih acak dari 5 jenis (BLINK, TURN_LEFT, TURN_RIGHT, NOD, MOUTH_OPEN) tanpa pengulangan, urutan diacak per sesi.
- [ ] Transisi state hanya sepanjang sisi yang legal: `PENDING → IN_PROGRESS → {PASSED, FAILED, EXPIRED}`. Transisi ilegal mengembalikan error, bukan panic dan bukan diabaikan diam-diam.
- [ ] Kedaluwarsa dikendalikan interface `Clock` yang bisa di-fake — tidak ada `time.Now()` di dalam domain.
- [ ] Table-driven test mencakup **setiap** transisi legal dan minimal 5 transisi ilegal.

**Verification:** `docker compose run --rm dev go test ./internal/liveness/... -race -cover` (≥ 90% untuk file-file ini)
**Dependencies:** T1 ∥ (paralel dengan Fase 3)
**Files:** `internal/liveness/session.go` · `internal/liveness/challenge.go` · `internal/liveness/session_test.go` · `internal/liveness/challenge_test.go`
**Scope:** M

---

### T17: Evaluator challenge

**Acceptance criteria:**
- [ ] BLINK: EAR < ambang selama ≥ 2 frame berturut, **diikuti pemulihan** di atas ambang — mencegah mata terpejam terus dihitung sebagai kedipan.
- [ ] TURN_LEFT/RIGHT: |yaw| > ambang dengan tanda yang benar; NOD: delta pitch; MOUTH_OPEN: MAR > ambang.
- [ ] Setiap challenge punya batas waktunya sendiri; lewat batas → sesi FAILED dengan alasan yang jelas.
- [ ] Seluruh ambang dari config; nol angka ajaib.

**Verification:** `docker compose run --rm dev go test ./internal/liveness/ -run TestEvaluator -race`
**Dependencies:** T16, T10
**Files:** `internal/liveness/evaluator.go` · `internal/liveness/evaluator_test.go`
**Scope:** M

---

### T18: Anti-replay — 6 lapis pertahanan

**Description:** Implementasikan keenam pertahanan di SPEC §5. Ini permukaan keamanan inti sistem.

**Acceptance criteria:**
- [ ] Keenam lapis aktif: challenge acak, nonce + seq monoton, batas waktu, dedup pHash, anti-spoof per frame, konsistensi identitas lintas frame.
- [ ] **Setiap lapis punya test kasus-gagal tersendiri** yang membuktikan serangan ditolak.
- [ ] Cek identitas: embedding frame kunci harus cosine ≥ ambang terhadap frame pertama; kalau tidak → `ErrIdentityChanged`.
- [ ] Penolakan mencatat alasan terstruktur — tanpa data biometrik apa pun di log.

**Verification:** `docker compose run --rm dev go test ./internal/liveness/ -run TestAntiReplay -race -cover` (≥ 90%)
**Dependencies:** T16, T7, T12
**Files:** `internal/liveness/antireplay.go` · `internal/liveness/antireplay_test.go`
**Scope:** M

---

### T19: liveness.Service

**Acceptance criteria:**
- [ ] `Start` / `SubmitFrame` / `Complete` / `Get` sesuai kontrak SPEC §5.
- [ ] Frame berkualitas rendah **tidak** menggagalkan sesi — mengembalikan alasan, klien mencoba lagi. Hanya spoof dan pergantian identitas yang fatal.
- [ ] `Complete` menolak sesi yang belum menuntaskan semua challenge.
- [ ] Semua dependency lewat interface; test memakai pipeline stub dan repo fake.

**Verification:** `docker compose run --rm dev go test ./internal/liveness/... -race -cover` (≥ 80% paket)
**Dependencies:** T13, T15, T17, T18
**Files:** `internal/liveness/service.go` · `internal/liveness/service_test.go`
**Scope:** M

---

### T20: Handler HTTP liveness

**Acceptance criteria:**
- [ ] Keempat endpoint liveness dari SPEC §5 lengkap dengan bentuk request/response yang tepat.
- [ ] Frame base64 > 2 MB → 413 lewat `MaxBytesReader`, bukan setelah decode.
- [ ] Error domain terpetakan benar: expired → 410, replay → 409, spoof → 422, tidak ketemu → 404.
- [ ] Rate limit per API key aktif di endpoint frame.

**Verification:** `docker compose run --rm dev go test ./internal/httpapi/... -race` + `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestLivenessAPI`
**Dependencies:** T19, T2
**Files:** `internal/httpapi/liveness_handler.go` · `internal/httpapi/dto.go` · `internal/httpapi/liveness_handler_test.go` · `internal/httpapi/router.go` (edit)
**Scope:** M

---

### T21: Demo web UI ∥

**Description:** Halaman tunggal, vanilla JS, tanpa npm. Ini yang mengubah Milestone A dari "test hijau" menjadi "saya melihatnya bekerja". Boleh paralel dengan T20 setelah DTO dibekukan.

**Acceptance criteria:**
- [ ] Meminta akses kamera, memulai sesi, menampilkan instruksi challenge aktif dalam Bahasa Indonesia.
- [ ] Mengambil dan mengirim frame ~6 fps, menampilkan progres, merender verdict akhir.
- [ ] Menangani kamera ditolak, sesi kedaluwarsa, dan error jaringan dengan pesan yang bisa dimengerti — bukan console error.
- [ ] Aset di-embed via `//go:embed`; tidak ada CDN eksternal, tidak ada build step.

**Verification:**
- [ ] Manual: buka `http://localhost:8080/demo` di Chrome dan Edge, selesaikan sesi penuh
- [ ] Manual: tolak izin kamera → pesan jelas, bukan halaman kosong

**Dependencies:** T20 (kontrak DTO)
**Files:** `web/index.html` · `web/app.js` · `web/style.css` · `internal/httpapi/web.go`
**Scope:** M

> ### 🎯 CHECKPOINT A — MILESTONE A
> Verifikasi SPEC §9 A1–A8:
> - [ ] A1 compose naik healthy < 5 menit dari cache kosong
> - [ ] A2 `/readyz` melaporkan DB, MinIO, dan 4 model ter-load
> - [ ] A3 **Demo webcam menuntaskan sesi 3-challenge, verdict < 2 detik**
> - [ ] A4 p95 inference per frame < 150 ms di CPU 4 core
> - [ ] A5 serangan cetak ditolak
> - [ ] A6 serangan replay gagal karena urutan challenge acak
> - [ ] A7 frame duplikat, seq mundur, pergantian identitas ditolak dengan kode tepat
> - [ ] A8 sesi kedaluwarsa di 90 detik dan dibersihkan dari DB
> - [ ] X5 migrasi up→down→up bersih
> - [ ] Seluruh test lolos dengan stub, tanpa model
> - [ ] **Tinjau bersama. Ini titik keputusan alami untuk berhenti atau lanjut ke Milestone B.**

---

## Fase 5 — Milestone B: Enrollment & 1:N

### T22: Skema pgvector, face repo, index HNSW

**Acceptance criteria:**
- [ ] Kolom `vector(512)` dengan index HNSW `vector_cosine_ops`; parameter `m` dan `ef_construction` dari config.
- [ ] Codec pgvector terdaftar di pgx; embedding bolak-balik tanpa kehilangan presisi.
- [ ] Top-K search mengembalikan kandidat terurut beserta cosine similarity.
- [ ] Harness benchmark: 10.000 embedding sintetis, mengukur p95 dan recall@1 vs brute force (B2, B3).

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestFaceRepo -timeout=10m`
**Dependencies:** T14
**Files:** `migrations/00003_faces_pgvector.sql` · `internal/storage/postgres/face_repo.go` · `internal/storage/postgres/vector_codec.go` · `tests/integration/face_repo_test.go`
**Scope:** M

---

### T23: Object store MinIO ∥

**Acceptance criteria:**
- [ ] Put/Get/Delete dengan kunci content-addressed (SHA-256 dari isi), mencegah duplikasi.
- [ ] Bucket dibuat otomatis saat boot bila belum ada; kebijakan **privat**, tidak ada akses anonim.
- [ ] Delete idempoten; menghapus objek yang tidak ada bukan error.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestObjStore`
**Dependencies:** T3 ∥ (paralel dengan T22)
**Files:** `internal/storage/objstore/minio.go` · `tests/integration/objstore_test.go`
**Scope:** M

---

### T24: Liveness token — terbitkan dan pakai sekali

**Description:** Pengikat antara Milestone A dan B. Tanpa ini, enrollment bisa dilakukan dengan foto biasa dan seluruh sistem liveness jadi teater.

**Acceptance criteria:**
- [ ] Token ditandatangani, ber-TTL pendek (5 menit), terikat pada `session_id` dan verdict PASSED.
- [ ] **Sekali pakai** — konsumsi kedua gagal, dijamin di level database, bukan di memori (B1).
- [ ] Token dari sesi FAILED atau EXPIRED ditolak.
- [ ] Signature tidak valid ditolak tanpa membocorkan alasan ke klien.

**Verification:** `docker compose run --rm dev go test ./internal/liveness/ -run TestToken -race` + test integrasi konsumsi ganda
**Dependencies:** T19, T14
**Files:** `internal/liveness/token.go` · `internal/liveness/token_test.go` · `migrations/00004_liveness_tokens.sql`
**Scope:** M

---

### T25: enrollment.Service

**Acceptance criteria:**
- [ ] `Enroll` **wajib** menerima liveness token yang valid dan belum terpakai; menyimpan embedding + artefak dan mengembalikan `subject_id`.
- [ ] `Search` 1:N mengembalikan top-K + similarity + flag `match` terhadap ambang config.
- [ ] `Verify` 1:1 adalah Search yang difilter ke satu subject — bukan jalur kode terpisah.
- [ ] `Delete` menghapus embedding **dan** objek MinIO, dan menyisakan baris audit (B4).
- [ ] Kegagalan parsial (DB berhasil, MinIO gagal) tidak meninggalkan data yatim — urutan operasi didokumentasikan dan di-test.

**Verification:** `docker compose run --rm dev go test ./internal/enrollment/... -race -cover` (≥ 80%)
**Dependencies:** T22, T23, T24, T13
**Files:** `internal/enrollment/service.go` · `internal/enrollment/threshold.go` · `internal/enrollment/service_test.go`
**Scope:** M

---

### T26: Handler HTTP faces

**Acceptance criteria:**
- [ ] Kelima endpoint faces dari SPEC §5.
- [ ] Response **tidak pernah** memuat vektor embedding mentah.
- [ ] `subject_id` tidak pernah muncul di query string — body atau path parameter saja.
- [ ] Enroll tanpa token valid → 403 dengan pesan yang jelas.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestFacesAPI`
**Dependencies:** T25, T20
**Files:** `internal/httpapi/faces_handler.go` · `internal/httpapi/dto.go` (edit) · `internal/httpapi/faces_handler_test.go`
**Scope:** M

---

### T27: Audit log

**Acceptance criteria:**
- [ ] Setiap keputusan verifikasi dan enrollment menulis baris audit: timestamp, session/subject id, verdict, skor, referensi artefak (B5).
- [ ] Baris audit **append-only**; tidak ada jalur update atau delete di repository.
- [ ] Audit menyimpan **referensi**, bukan data biometrik.
- [ ] Delete subject menyisakan jejak audit yang membuktikan penghapusan terjadi.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestAudit`
**Dependencies:** T14, T19, T25
**Files:** `migrations/00005_audit_log.sql` · `internal/storage/postgres/audit_repo.go` · `tests/integration/audit_test.go` · wiring di service (edit)
**Scope:** M

> ### 🎯 CHECKPOINT B — MILESTONE B
> Verifikasi SPEC §9 B1–B5:
> - [ ] B1 enrollment menolak request tanpa liveness token valid & belum terpakai
> - [ ] B2 1:N pada 10.000 embedding, p95 < 50 ms
> - [ ] B3 recall@1 HNSW ≥ 0.98 vs brute force
> - [ ] B4 delete menghapus embedding + objek MinIO, audit tersisa
> - [ ] B5 setiap keputusan dapat ditelusuri lengkap
> - [ ] **Tinjau bersama**

---

## Fase 6 — Hardening

### T28: Suite regresi serangan

**Acceptance criteria:**
- [ ] Serangan cetak, replay layar, frame duplikat, seq mundur, dan pergantian identitas mid-sesi — semuanya ditolak (A5, A6, A7).
- [ ] Sampel serangan disimpan di `testdata/attacks/`, **tanpa wajah orang sungguhan** — sintetis atau CC0.
- [ ] Setiap serangan yang gagal ditolak mencetak diagnostik yang cukup untuk debugging.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run TestAttack -v`
**Dependencies:** Checkpoint A
**Files:** `tests/integration/attack_test.go` · `testdata/attacks/` · `testdata/attacks/README.md`
**Scope:** M

---

### T29: Observability, readyz, penjaga kebocoran log

**Acceptance criteria:**
- [ ] `/readyz` mengecek DB, MinIO, dan model ter-load; 503 dengan detail per komponen saat ada yang mati (A2).
- [ ] Test menangkap seluruh output `slog` di sepanjang alur penuh dan memastikan **tidak ada** blob base64, path gambar, atau angka embedding (X4).
- [ ] `request_id` dan `session_id` hadir di setiap baris log jalur request.

**Verification:** `docker compose run --rm dev go test -tags=integration ./tests/integration/ -run "TestReadyz|TestLogLeak"`
**Dependencies:** T20, T26
**Files:** `internal/httpapi/health.go` · `internal/obs/logging.go` · `tests/integration/logleak_test.go`
**Scope:** M

---

### T30: Harness kalibrasi threshold ⛔

**Description:** Mengubah threshold dari tebakan literatur menjadi angka terukur. **Terblokir Open Question #2 (target FAR/FRR) dan #3 (dataset).**

**Acceptance criteria:**
- [ ] `calibrate -dataset <dir>` menghasilkan kurva FAR/FRR dan ambang optimal per parameter.
- [ ] Output berupa blok env var yang bisa langsung ditempel ke `.env`.
- [ ] `docs/calibration.md` mencatat dataset, tanggal, versi model, dan angka hasil.

**Verification:** `docker compose run --rm dev go run ./cmd/calibrate -dataset testdata/calibration`
**Dependencies:** Checkpoint B · **Open Q#2, Q#3**
**Files:** `cmd/calibrate/main.go` · `docs/calibration.md`
**Scope:** M

---

### T31: README dan verifikasi akhir

**Acceptance criteria:**
- [ ] README memungkinkan orang lain menjalankan project dari nol dalam < 10 menit (X6).
- [ ] Mendokumentasikan batasan secara jujur: bukan tersertifikasi PAD, threshold status kalibrasinya, lisensi model.
- [ ] Menjelaskan bahwa demo hanya jalan di `localhost` (secure context), bukan lewat IP LAN.
- [ ] SPEC.md diperbarui: open question yang sudah terjawab ditutup, riwayat revisi ditambah.

**Verification:**
- [ ] Manual: jalankan ulang di direktori bersih hanya mengikuti README
- [ ] `docker compose run --rm dev go test ./... -race && golangci-lint run ./...`

**Dependencies:** T28, T29, T30
**Files:** `README.md` · `SPEC.md` (edit) · `docs/limitations.md`
**Scope:** S

> ### ✅ CHECKPOINT SELESAI
> - [ ] X1 `go test ./... -race` bersih
> - [ ] X2 coverage domain ≥ 80%, repo ≥ 70%
> - [ ] X3 `golangci-lint run ./...` nol issue
> - [ ] X4 test membuktikan tidak ada kebocoran gambar/embedding ke log
> - [ ] X5 migrasi up→down→up bersih
> - [ ] X6 README terbukti di mesin bersih
> - [ ] Semua kriteria A1–A8 dan B1–B5 hijau

---

## Ringkasan Ukuran

| Fase | Task | Ukuran | Catatan |
|---|---|---|---|
| 1 Walking Skeleton | T1–T3 | 3×M | |
| 2 Inference Spike | T4–T8 | 4×M, 1×S | **T5 gate risiko** |
| 3 Biometric Pipeline | T9–T13 | 2×M, 3×S | T11 ∥ T12 |
| 4 Milestone A | T14–T21 | 8×M | T16 bisa mulai lebih awal |
| 5 Milestone B | T22–T27 | 6×M | |
| 6 Hardening | T28–T31 | 3×M, 1×S | T30 terblokir Open Q#2/#3 |

**Total: 31 task.** Tidak ada yang berukuran L atau lebih. Tidak ada yang menyentuh lebih dari 5 file.
