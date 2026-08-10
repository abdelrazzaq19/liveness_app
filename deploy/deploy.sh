#!/usr/bin/env bash
#
# Menerbitkan satu rilis, lalu mengembalikannya kalau tidak sehat.
#
#   deploy.sh <release-dir> <image-tag>
#
# Lingkungan yang wajib:
#   DEPLOY_ROOT       akar rilis; berisi releases/ dan symlink current
#   DEPLOY_ENV_FILE   .env untuk lingkungan itu; TIDAK pernah datang dari repo
#
# Opsional:
#   COMPOSE_PROJECT   nama project compose            (default: liveness)
#   HEALTH_URL        yang di-poll setelah swap       (default: /readyz lokal)
#   HEALTH_TIMEOUT    detik menunggu sehat            (default: 90)
#
# ── Apa yang dibalikkan rollback, dan apa yang tidak ────────────────────────
#
# Rollback membalikkan KODE dan IMAGE. Ia tidak pernah membalikkan SKEMA.
#
# Itu bukan kemalasan, melainkan satu-satunya pilihan yang aman. Migrasi 00003
# menambahkan kolom `retries`, dan kolom itu memegang nilai sungguhan begitu ada
# sesi yang berjalan. Menurunkannya untuk membatalkan sebuah rilis akan membawa
# nilai-nilai itu ikut hilang — kerugian permanen demi memperbaiki masalah yang
# sementara.
#
# Yang membuat ini bekerja adalah aturan yang harus dipegang: SETIAP MIGRASI
# HARUS KOMPATIBEL MUNDUR. Kolom baru punya default, tabel baru tidak dipakai
# versi lama, tidak ada kolom yang dihapus atau diganti nama dalam satu rilis.
# Selama itu dipegang, versi lama berjalan baik-baik saja di atas skema baru,
# dan rollback kode saja sudah cukup.
#
# VerifySchema di server menolak skema yang LEBIH TUA dari yang dibutuhkan
# binernya, bukan yang lebih baru — jadi versi lama di atas skema baru memang
# lolos readiness, dan itu disengaja.
#
# Kalau sebuah rilis suatu saat butuh migrasi yang merusak, rilis itu tidak
# boleh memakai skrip ini. Ia butuh rencana tersendiri, ditulis sebelumnya.
set -euo pipefail

RELEASE_DIR=${1:?release dir is required}
IMAGE_TAG=${2:?image tag is required}

: "${DEPLOY_ROOT:?DEPLOY_ROOT is required}"
: "${DEPLOY_ENV_FILE:?DEPLOY_ENV_FILE is required}"

COMPOSE_PROJECT=${COMPOSE_PROJECT:-liveness}
HEALTH_URL=${HEALTH_URL:-http://127.0.0.1:8080/readyz}
HEALTH_TIMEOUT=${HEALTH_TIMEOUT:-90}

CURRENT_LINK="$DEPLOY_ROOT/current"

log() { printf '[deploy] %s\n' "$*"; }

compose() {
    local dir=$1
    shift
    docker compose \
        --project-name "$COMPOSE_PROJECT" \
        --env-file "$DEPLOY_ENV_FILE" \
        --file "$dir/compose.yaml" \
        "$@"
}

# wait_healthy memoll /readyz sampai siap atau waktunya habis.
#
# /readyz, bukan /healthz. Yang kedua hanya berarti prosesnya hidup, dan sebuah
# rilis yang hidup dengan database tak terjangkau adalah persis rilis yang harus
# dibatalkan.
wait_healthy() {
    local deadline=$(( SECONDS + HEALTH_TIMEOUT ))

    while [ "$SECONDS" -lt "$deadline" ]; do
        if curl -fsS --max-time 5 "$HEALTH_URL" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# ── Sebelum menyentuh apa pun ───────────────────────────────────────────────

test -d "$RELEASE_DIR"
test -f "$RELEASE_DIR/compose.yaml"
test -r "$DEPLOY_ENV_FILE"
command -v curl >/dev/null
mkdir -p "$DEPLOY_ROOT/releases"

# --env-file memberi variabel kepada interpolasi compose, bukan kepada
# container. Service api membaca `env_file: .env` relatif terhadap compose
# file-nya, dan direktori rilis tidak punya satu pun — .env sengaja tidak ikut
# disalin dari repo.
#
# Symlink, bukan salinan. Secret tetap hidup di satu tempat, dan menghapus
# rilis lama tidak menyebarkan tembusannya ke mana-mana.
ln -sfn "$DEPLOY_ENV_FILE" "$RELEASE_DIR/.env"

# Rilis sebelumnya dicatat SEBELUM apa pun berubah. Tanpa ini rollback tidak
# punya tujuan, dan menemukan itu setelah deploy gagal adalah waktu yang paling
# buruk untuk menemukannya.
PREVIOUS_DIR=""
if [ -L "$CURRENT_LINK" ]; then
    PREVIOUS_DIR=$(readlink -f "$CURRENT_LINK" || true)
fi

if [ -n "$PREVIOUS_DIR" ]; then
    log "rilis sebelumnya: $(basename "$PREVIOUS_DIR")"
else
    log "tidak ada rilis sebelumnya; ini penerbitan pertama"
    log "PERINGATAN: rollback tidak tersedia untuk deploy ini"
fi

# ── Migrasi, maju saja ──────────────────────────────────────────────────────
#
# Dijalankan sebelum versi baru menerima trafik, dan sebagai perintah tersendiri
# bukan sesuatu yang server lakukan saat boot. Server yang bermigrasi saat naik
# berlomba dengan dirinya sendiri begitu ada lebih dari satu replika.

log "menerapkan migrasi"
# Tanpa --no-deps: migrasi butuh databasenya hidup, dan service api sudah
# mendeklarasikan depends_on dengan kondisi healthy. Compose yang menyalakan
# dan menunggunya, jadi skrip ini tidak perlu punya pendapat sendiri tentang
# urutan start.
compose "$RELEASE_DIR" run --rm api -migrate

# ── Terbitkan ───────────────────────────────────────────────────────────────

log "membangun image $IMAGE_TAG"
compose "$RELEASE_DIR" build api

log "mengarahkan current ke $IMAGE_TAG"
# Symlink diganti secara atomik lewat rename, bukan dihapus lalu dibuat. Jeda
# antara hapus dan buat adalah jeda ketika current tidak menunjuk ke mana pun.
ln -sfn "$RELEASE_DIR" "$CURRENT_LINK.tmp"
mv -Tf "$CURRENT_LINK.tmp" "$CURRENT_LINK"

log "menyalakan"
compose "$RELEASE_DIR" up -d api

# ── Gerbang kesehatan ───────────────────────────────────────────────────────

log "menunggu $HEALTH_URL, batas ${HEALTH_TIMEOUT}s"

if wait_healthy; then
    log "sehat; rilis $IMAGE_TAG diterbitkan"
    exit 0
fi

# ── Rollback ────────────────────────────────────────────────────────────────

log "TIDAK sehat dalam ${HEALTH_TIMEOUT}s"

if [ -z "$PREVIOUS_DIR" ] || [ ! -d "$PREVIOUS_DIR" ]; then
    log "tidak ada rilis sebelumnya untuk dikembalikan"
    log "layanan tetap pada $IMAGE_TAG dan TIDAK sehat — perlu penanganan manual"
    exit 1
fi

log "mengembalikan ke $(basename "$PREVIOUS_DIR")"

ln -sfn "$PREVIOUS_DIR" "$CURRENT_LINK.tmp"
mv -Tf "$CURRENT_LINK.tmp" "$CURRENT_LINK"

compose "$PREVIOUS_DIR" up -d api

if wait_healthy; then
    log "rollback selesai; layanan sehat pada $(basename "$PREVIOUS_DIR")"
    # Tetap keluar non-zero. Rilis ini gagal, dan build hijau setelah rollback
    # yang berhasil akan menyembunyikan justru hal yang perlu diperbaiki.
    exit 1
fi

log "rollback TIDAK memulihkan kesehatan — perlu penanganan manual"
exit 1
