# Menjalankan deploy pertama kali

Pipeline-nya sudah ada dan logikanya sudah diuji. Yang **belum** pernah terjadi
adalah deploy sungguhan ke VPS. Dokumen ini urutan untuk melakukannya sekali,
dengan aman, dan membuktikan rollback-nya bekerja sebelum Anda perlu
mengandalkannya.

---

## 1. Siapkan VPS

Semua ini di agent yang dilabeli `development-vps`, sebagai user yang
menjalankan Jenkins.

### Yang harus ada

```bash
docker version && docker compose version
command -v rsync curl
```

`rsync` dan `curl` dipakai pipeline. Kalau salah satu tidak ada, stage
Preflight-nya gagal dengan jelas — tapi lebih baik dipasang sekarang.

### Direktori rilis

```bash
sudo mkdir -p /opt/liveness/development/releases
sudo chown -R "$(whoami)":"$(whoami)" /opt/liveness/development
```

Sesuaikan `DEPLOY_ROOT` di `Jenkinsfile` kalau Anda memakai path lain.

### Berkas lingkungan

Ini **tidak pernah** datang dari repo. Ia tinggal di host, dan `deploy.sh`
mem-symlink-nya ke dalam direktori rilis.

```bash
sudo -e /opt/liveness/development/.env
```

Sebelas variabel. Enam wajib untuk aplikasi, lima untuk interpolasi compose:

```sh
# Interpolasi compose
POSTGRES_USER=liveness
POSTGRES_PASSWORD=<ganti>
POSTGRES_DB=liveness
MINIO_ROOT_USER=liveness-minio
MINIO_ROOT_PASSWORD=<ganti-minimal-8-karakter>

# Wajib untuk aplikasi — service menolak boot tanpa ini
LV_API_KEYS=<ganti>
LV_DATABASE_URL=postgres://liveness:<password-postgres>@postgres:5432/liveness?sslmode=disable
LV_OBJSTORE_ENDPOINT=minio:9000
LV_OBJSTORE_ACCESS_KEY=liveness-minio
LV_OBJSTORE_SECRET_KEY=<password-minio-yang-sama>
LV_TOKEN_SECRET=<ganti>

# Pilihan sadar, bukan default
LV_PIPELINE_MODE=onnx
LV_ALLOW_ANONYMOUS_SESSIONS=false
LV_LIVENESS_ANTISPOOF_ENFORCE=false
```

Bangkitkan setiap `<ganti>` dengan:

```bash
openssl rand -hex 32
```

Lalu kunci:

```bash
chmod 600 /opt/liveness/development/.env
```

> **`LV_LIVENESS_ANTISPOOF_ENFORCE=false` adalah keputusan, bukan kelalaian.**
> Dengan `true`, model yang dipakai sekarang menolak **setiap** subjek asli.
> Dengan `false`, foto cetak dan replay layar tidak diblokir. Server
> memperingatkan di setiap boot. Lihat README bagian Status.

### Model

`LV_PIPELINE_MODE=onnx` menuntut berkas `.onnx` ada di host. Kalau belum:

```bash
cd /opt/liveness/development
docker compose --profile setup run --rm modelctl download
docker compose --profile setup run --rm modelctl verify
```

Atau jalankan dengan `LV_PIPELINE_MODE=stub` lebih dulu untuk membuktikan
pipeline-nya, lalu ganti. Stub tidak mengukur apa pun tentang wajah, dan server
mengatakannya di setiap boot.

### Port

Deploy mem-bind `127.0.0.1:8080`. Pastikan kosong:

```bash
ss -ltnp | grep 8080 || echo "8080 bebas"
```

---

## 2. Siapkan job Jenkins

**New Item → Pipeline** (atau Multibranch Pipeline).

| Setelan | Nilai |
|---|---|
| Definition | Pipeline script from SCM |
| SCM | Git, repo ini |
| Branch | `*/development` |
| Script Path | `Jenkinsfile` |

Untuk Pipeline biasa (bukan Multibranch), `BRANCH_NAME` tidak terisi sendiri.
Guard branch akan menolak. Tambahkan parameter string `BRANCH_NAME` bernilai
`development`, atau pakai Multibranch yang mengisinya otomatis.

---

## 3. Jalankan pertama — TANPA deploy

Jangan langsung `DEPLOY=true`.

```
Build with Parameters
  DEPLOY          = false      ← biarkan
  RUN_MODEL_TESTS = false
```

Yang harus terjadi: Preflight, image dev, gerbang kualitas, tiga lapis test,
build image runtime. Selesai hijau, tanpa menyentuh apa pun yang hidup.

Kalau ini gagal, perbaiki dulu. Deploy di atas pipeline yang belum terbukti
hanya menambah satu variabel yang tidak perlu.

---

## 4. Deploy pertama

```
Build with Parameters
  DEPLOY = true
```

Yang akan Anda lihat di log:

```
[deploy] tidak ada rilis sebelumnya; ini penerbitan pertama
[deploy] PERINGATAN: rollback tidak tersedia untuk deploy ini
[deploy] menerapkan migrasi
schema moved from version 0 to 6
[deploy] membangun image <sha>
[deploy] mengarahkan current ke <sha>
[deploy] menyalakan
[deploy] menunggu http://127.0.0.1:8080/readyz, batas 120s
[deploy] sehat; rilis <sha> diterbitkan
```

**Peringatan "rollback tidak tersedia" itu benar dan penting.** Deploy pertama
tidak punya rilis sebelumnya untuk dituju. Kalau gerbang kesehatannya gagal,
layanan tetap pada rilis yang rusak dan butuh penanganan manual — skrip
mengatakannya, tidak berpura-pura.

Periksa dari VPS:

```bash
curl -s http://127.0.0.1:8080/healthz
curl -s http://127.0.0.1:8080/readyz
ls -l /opt/liveness/development/current
```

---

## 5. Buktikan rollback — sebelum Anda membutuhkannya

Rollback yang belum pernah dijalankan adalah rollback yang belum ada. Lakukan
sekali, sengaja, saat tidak ada yang bergantung padanya.

Buat commit di branch `development` yang merusak kesiapan **tanpa** merusak
build. Cara paling bersih: arahkan database ke host yang tidak ada.

```bash
git checkout development
```

Tambahkan di `compose.yaml`, di dalam `services.api`:

```yaml
    environment:
      LV_DATABASE_URL: postgres://liveness:x@tidak-ada-host:5432/liveness?sslmode=disable
```

Commit, push, lalu jalankan dengan `DEPLOY=true`.

Yang harus terjadi:

```
[deploy] rilis sebelumnya: <sha-lama>
[deploy] menerapkan migrasi          ← ini akan GAGAL
```

Migrasi gagal lebih dulu, jadi rilis rusak itu tidak pernah sampai ke gerbang
kesehatan. Itu perilaku yang benar — dan berarti untuk menguji **rollback**,
kerusakannya harus lolos migrasi tapi jatuh di kesiapan.

Cara yang menguji jalur itu: buat `/readyz` gagal tanpa menyentuh database.
Paling sederhana, ubah `ExpectedSchemaVersion` di
`internal/storage/postgres/db.go` menjadi `99`:

```go
const ExpectedSchemaVersion = 99
```

Skema sungguhan ada di 6, jadi `VerifySchema` menolak, `/readyz` menjawab 503,
dan gerbang kesehatan jatuh setelah migrasi lewat. Yang harus terjadi:

```
[deploy] menunggu http://127.0.0.1:8080/readyz, batas 120s
[deploy] TIDAK sehat dalam 120s
[deploy] mengembalikan ke <sha-lama>
[deploy] rollback selesai; layanan sehat pada <sha-lama>
```

Dan build-nya **merah**, meskipun rollback-nya berhasil. Itu disengaja: build
hijau setelah rollback menyembunyikan justru hal yang perlu diperbaiki.

Periksa layanan benar-benar pulih:

```bash
curl -s http://127.0.0.1:8080/readyz    # {"status":"ready"}
readlink /opt/liveness/development/current
```

Lalu kembalikan commit percobaannya.

---

## 6. Apa yang TIDAK dilakukan pipeline ini

Ditulis di sini supaya tidak ada yang mengiranya melakukan lebih dari yang ia
lakukan.

- **Tidak membalikkan skema.** Rollback membalikkan kode dan image. Alasannya
  lengkap di [deploy/deploy.sh](../deploy/deploy.sh); ringkasnya, menurunkan
  migrasi yang menambah kolom akan membuang data yang ada di dalamnya.
- **Tidak menangani migrasi yang merusak.** Rilis yang butuh menghapus atau
  mengganti nama kolom tidak boleh memakai skrip ini. Ia butuh rencana
  tersendiri, ditulis sebelumnya.
- **Tidak zero-downtime.** `compose up -d api` mengganti container. Ada jeda
  beberapa detik. Untuk lingkungan development itu memadai.
- **Tidak men-deploy ke production.** Hanya satu lingkungan, satu branch.
- **Tidak memangkas image Docker lama.** Rilis dipangkas menyisakan lima; image
  menumpuk. Jalankan `docker image prune` secara berkala, atau tambahkan
  stage-nya kalau disknya mulai jadi masalah.

---

## Kalau ada yang salah

**`test "${BRANCH_NAME:-}" = "$DEPLOY_BRANCH"` gagal** — job-nya Pipeline biasa,
bukan Multibranch. Lihat bagian 2.

**`LV_API_KEYS: is required but not set`** — `.env` di host tidak terbaca.
Periksa `DEPLOY_ENV_FILE` menunjuk ke berkas yang ada dan bisa dibaca user
Jenkins.

**Migrasi gagal dengan `hostname resolving error`** — DSN menunjuk ke host yang
tidak ada di jaringan compose. Di dalam compose, hostnya `postgres`, bukan
`localhost`.

**`/readyz` menjawab 503 terus** — periksa logya:

```bash
docker compose -p liveness --env-file /opt/liveness/development/.env \
  -f /opt/liveness/development/current/compose.yaml logs api --tail 50
```

`/readyz` menyebut **nama** cek yang gagal; alasannya ada di log.
