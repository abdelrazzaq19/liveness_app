// Pipeline CI untuk Liveness Verifier.
//
// Kerangkanya mengikuti pipeline pintarin — guard branch, preflight, mount
// workspace read-only, cache Go di volume bernama, set -eu di mana-mana —
// karena pola-pola itu memang benar dan menyamakannya berarti orang yang sudah
// membaca yang satu langsung mengerti yang lain.
//
// Empat hal berbeda, dan semuanya karena project ini berbeda:
//
//   1. Satu service, bukan tiga. Tidak ada daftar service yang di-hardcode,
//      jadi tidak ada cara menambah paket baru yang diam-diam tidak pernah
//      dites. `go test ./...` mencakup semuanya menurut konstruksi.
//
//   2. Ada gerbang kualitas: gofumpt, go vet pada tiga build tag, dan
//      golangci-lint. Test saja tidak menangkap kode yang tidak dikompilasi di
//      bawah tag `models` atau `integration`.
//
//   3. Tiga lapis test dengan kebutuhan berbeda, dijalankan terpisah supaya
//      kegagalannya menyebutkan lapis mana yang jatuh:
//        unit + attack  -> tidak butuh apa-apa (pipeline stub)
//        integration    -> Postgres + MinIO
//        models         -> file .onnx yang tidak di-commit
//
//   4. Pipeline ini TIDAK men-deploy. Ia berhenti setelah image terbangun.
//      Menambahkan deploy berarti menambahkan rollback, dan rollback butuh
//      keputusan yang belum diambil siapa pun.
pipeline {
    agent { label 'development-vps' }

    options {
        buildDiscarder(logRotator(numToKeepStr: '20'))
        disableConcurrentBuilds()
        skipDefaultCheckout(true)
        timestamps()
        timeout(time: 45, unit: 'MINUTES')
    }

    parameters {
        // Mati secara default karena model tidak ikut di-commit dan lisensinya
        // riset non-komersial. Nyalakan hanya pada agent yang benar-benar punya
        // filenya; lihat models/README.md.
        booleanParam(
            name: 'RUN_MODEL_TESTS',
            defaultValue: false,
            description: 'Jalankan test bertag `models` terhadap file .onnx asli. Butuh models/ terisi di agent.'
        )
    }

    environment {
        // Satu project compose per build, supaya CI tidak bertabrakan dengan
        // stack development yang mungkin hidup di host yang sama.
        COMPOSE_PROJECT = "liveness-ci-${env.BUILD_NUMBER}"
        COMPOSE_FILES   = '-f compose.yaml -f compose.ci.yaml'

        GO_IMAGE = 'golang:1.23-bookworm'
    }

    stages {
        stage('Checkout') {
            steps {
                checkout scm
                script {
                    env.IMAGE_TAG = sh(
                        returnStdout: true,
                        script: 'git rev-parse --verify HEAD'
                    ).trim()
                }
            }
        }

        stage('Preflight') {
            steps {
                sh '''
                    set -eu
                    docker version
                    docker compose version

                    # Overlay CI harus ada sebelum apa pun mencoba memakainya.
                    test -f compose.ci.yaml

                    # Konfigurasi test: nilai buangan, bukan kredensial mana pun
                    # yang dipakai di luar build ini. Ditulis di sini alih-alih
                    # dibaca dari host, supaya build tidak pernah menyentuh
                    # secret deployment.
                    cat > .env <<'ENV'
POSTGRES_USER=liveness
POSTGRES_PASSWORD=ci-throwaway-postgres
POSTGRES_DB=liveness
MINIO_ROOT_USER=liveness-minio
MINIO_ROOT_PASSWORD=ci-throwaway-minio
LV_API_KEYS=ci-throwaway-api-key
LV_DATABASE_URL=postgres://liveness:ci-throwaway-postgres@postgres:5432/liveness?sslmode=disable
LV_OBJSTORE_ENDPOINT=minio:9000
LV_OBJSTORE_ACCESS_KEY=liveness-minio
LV_OBJSTORE_SECRET_KEY=ci-throwaway-minio
LV_TOKEN_SECRET=ci-throwaway-token-secret
LV_PIPELINE_MODE=stub
ENV

                    # Konfigurasi compose divalidasi sebelum ada yang dibangun,
                    # jadi salah ketik di YAML gagal di sini dan bukan tiga
                    # stage kemudian.
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES config --quiet
                '''
            }
        }

        stage('Image dev') {
            steps {
                sh '''
                    set -eu
                    # Gerbang kualitas dan test integrasi berjalan di image ini:
                    # ia yang memegang gofumpt, golangci-lint, dan
                    # libonnxruntime.so. Dibangun sekali, dipakai kedua stage.
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES build dev
                '''
            }
        }

        stage('Gerbang kualitas') {
            steps {
                sh '''
                    set -eu
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        run --rm --no-deps dev sh -ec '
                            # Berkas yang belum diformat dilaporkan namanya lalu
                            # build gagal. gofumpt -l tidak keluar non-zero
                            # sendiri, jadi keluarannya yang diperiksa.
                            unformatted=$(gofumpt -l .)
                            if [ -n "$unformatted" ]; then
                                echo "belum diformat:"
                                echo "$unformatted"
                                exit 1
                            fi

                            # go.mod yang sudah melenceng dari import-nya tetap
                            # bisa build, jadi drift-nya diperiksa terpisah.
                            cp go.mod /tmp/go.mod.before
                            cp go.sum /tmp/go.sum.before
                            go mod tidy
                            diff -u /tmp/go.mod.before go.mod
                            diff -u /tmp/go.sum.before go.sum

                            go mod verify

                            # Tiga tag, karena vet default tidak pernah melihat
                            # kode di bawah tag lain. Itulah cara test model dan
                            # integrasi berhenti dikompilasi tanpa ada yang tahu.
                            go vet ./...
                            go vet -tags=models ./...
                            go vet -tags=integration ./...

                            golangci-lint run ./...
                        '
                '''
            }
        }

        stage('Test — unit dan serangan') {
            steps {
                sh '''
                    set -eu
                    mkdir -p build

                    # Image Go polos, bukan image dev: lapis ini sengaja tidak
                    # bergantung pada ONNX Runtime maupun pada database, dan
                    # menjalankannya di sini membuktikan itu setiap kali. Kalau
                    # suatu saat ia mulai membutuhkan salah satunya, stage ini
                    # yang jatuh lebih dulu.
                    #
                    # Workspace di-mount read-only supaya test tidak bisa
                    # mengubah sumber yang sedang diujinya. Keluaran ditulis ke
                    # $WORKSPACE/build dari sisi host, bukan lewat mount itu.
                    rc=0
                    docker run --rm \
                        --mount "type=bind,source=$WORKSPACE,target=/src,readonly" \
                        --mount type=volume,source=liveness-go-mod-cache,target=/go/pkg/mod \
                        --mount type=volume,source=liveness-go-build-cache,target=/root/.cache/go-build \
                        --workdir /src \
                        "$GO_IMAGE" \
                        go test -race -count=1 -json ./... > build/test-unit.json || rc=$?

                    # Konversi ke JUnit dilakukan APA PUN hasilnya. Build yang
                    # gagal justru yang paling butuh laporannya, dan konversi
                    # yang hanya jalan saat hijau tidak berguna.
                    #
                    # Kode keluarnya sengaja diabaikan di sini, dan itu bukan
                    # kelalaian: `gotestsum --raw-command -- cat` meneruskan kode
                    # keluar `cat`, yang selalu nol. Mempercayainya berarti test
                    # merah dengan build hijau — kegagalan CI yang paling
                    # berbahaya karena tidak terlihat.
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        run --rm --no-deps dev \
                        gotestsum --junitfile build/test-unit.xml \
                        --raw-command -- cat build/test-unit.json || true

                    # Yang menentukan lulus atau tidak adalah kode keluar go
                    # test, yang ditangkap di atas.
                    exit $rc
                '''
            }
        }

        stage('Test — integrasi') {
            steps {
                sh '''
                    set -eu
                    mkdir -p build

                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        up -d --wait postgres minio

                    # Di sini gotestsum menjalankan go test sendiri, jadi kode
                    # keluarnya memang mencerminkan hasil test dan boleh
                    # dipercaya.
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        run --rm dev \
                        gotestsum --junitfile build/test-integration.xml --format testname \
                        -- -tags=integration -count=1 -timeout 20m ./tests/integration/...
                '''
            }
        }

        stage('Test — model') {
            when { expression { return params.RUN_MODEL_TESTS } }
            steps {
                sh '''
                    set -eu
                    mkdir -p build

                    # Dilewati dengan jelas, bukan gagal, kalau modelnya memang
                    # tidak ada di agent ini. Sebuah stage yang gagal karena
                    # sesuatu yang sengaja tidak di-commit mengajari orang
                    # mengabaikan kegagalan.
                    if [ -z "$(ls -A models/*.onnx 2>/dev/null)" ]; then
                        echo "models/ tidak berisi .onnx di agent ini; lapis ini dilewati."
                        echo "Lihat models/README.md — lisensinya riset non-komersial dan"
                        echo "filenya sengaja tidak di-commit."
                        exit 0
                    fi

                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        run --rm --no-deps dev \
                        gotestsum --junitfile build/test-models.xml --format testname \
                        -- -tags=models -count=1 -timeout 20m ./internal/biometric/onnx/...
                '''
            }
        }

        stage('Build image runtime') {
            steps {
                sh '''
                    set -eu
                    docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                        build --pull api
                '''
            }
        }
    }

    post {
        always {
            // Hasil test dipublikasikan sebagai JUnit, sehingga Jenkins bisa
            // menampilkan test mana yang jatuh dan trennya lintas build alih-alih
            // memaksa orang membaca log mentah.
            //
            // allowEmptyResults karena stage bisa saja tidak pernah berjalan —
            // gerbang kualitas yang gagal menghentikan pipeline sebelum test
            // apa pun dijalankan, dan itu bukan alasan untuk menandai build
            // sebagai tidak stabil karena laporannya kosong.
            junit testResults: 'build/*.xml', allowEmptyResults: true, skipPublishingChecks: true

            // JSON mentahnya ikut disimpan: ia memuat keluaran per test yang
            // tidak seluruhnya masuk ke XML.
            archiveArtifacts artifacts: 'build/*.json', allowEmptyArchive: true, fingerprint: false

            // Dirobohkan tanpa syarat, termasuk volume-nya. Container yang
            // tertinggal di agent akan menahan port dan disk sampai ada yang
            // menyadarinya, dan biasanya yang menyadarinya adalah build
            // berikutnya yang gagal karena alasan yang tidak berhubungan.
            sh '''
                docker compose --project-name "$COMPOSE_PROJECT" $COMPOSE_FILES \
                    down --volumes --remove-orphans || true

                # .env berisi kredensial buangan, tapi workspace bertahan antar
                # build di agent yang sama.
                rm -f .env
            '''
        }
    }
}
