package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"path/filepath"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/biometric/onnx"
	"github.com/ziad/liveness-verifier/internal/biometric/stub"
	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/enrollment"
	"github.com/ziad/liveness-verifier/internal/imaging"
	"github.com/ziad/liveness-verifier/internal/liveness"
	"github.com/ziad/liveness-verifier/internal/storage/objectstore"
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
)

// app holds everything that has to be shut down.
type app struct {
	db      *postgres.DB
	runtime *onnx.Runtime

	Liveness   *liveness.Service
	Enrollment *enrollment.Service
	Tokens     *enrollment.TokenService
}

// Close releases every resource, reporting the first failure but attempting all
// of them: a half-closed process is worse than a noisy shutdown.
func (a *app) Close() error {
	var err error
	if a.runtime != nil {
		if e := a.runtime.Close(); e != nil && err == nil {
			err = e
		}
	}
	if a.db != nil {
		a.db.Close()
	}
	return err
}

// build wires the whole application from configuration.
//
// This is the only function in the codebase that knows every package exists.
// Everything else receives what it needs.
func build(ctx context.Context, cfg *config.Config, log *slog.Logger) (*app, error) {
	a := &app{}

	db, err := postgres.Open(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	a.db = db

	sessions, err := postgres.NewSessionRepo(db)
	if err != nil {
		_ = a.Close()
		return nil, err
	}

	analyzer, runtime, err := buildAnalyzer(cfg, log)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.runtime = runtime

	svc, err := liveness.NewService(liveness.Deps{
		Sessions: sessions,
		Analyzer: analyzer,
		Evaluator: liveness.Evaluator{Thresholds: liveness.Thresholds{
			BlinkCloseRatio: cfg.Liveness.BlinkCloseRatio,
			BlinkOpenRatio:  cfg.Liveness.BlinkOpenRatio,
			BlinkMinFrames:  cfg.Liveness.BlinkMinFrames,
			YawTurnDeg:      cfg.Liveness.YawTurnDeg,
			PitchNodDeg:     cfg.Liveness.PitchNodDeg,
			MARMouthOpen:    cfg.Liveness.MARMouthOpen,
		}},
		Guard: liveness.Guard{
			MinLivenessScore:  cfg.Liveness.MinScore,
			EnforceAntiSpoof:  cfg.Liveness.EnforceAntiSpoof,
			IdentityCosineMin: cfg.Liveness.IdentityCosineMin,
			PHashMinDistance:  cfg.Imaging.PHashMinDistance,

			// A subject holding still trips the duplicate check, so tolerate a
			// couple of seconds of it at six frames a second before calling the
			// scene a photograph.
			MaxDuplicateStreak: 12,
			MaxRecentHashes:    64,
		},
		Clock:   liveness.SystemClock{},
		IDs:     randomIDs{},
		Entropy: rand.Reader,
		Logger:  log,
		Config: liveness.Config{
			TTL:                 cfg.Liveness.SessionTTL,
			ChallengeTimeout:    cfg.Liveness.ChallengeTimeout,
			ChallengeCount:      cfg.Liveness.ChallengeCount,
			MaxChallengeRetries: cfg.Liveness.MaxChallengeRetries,

			// The embedder is 71% of the pipeline. Re-checking identity every
			// fifth frame keeps the guarantee without the cost.
			KeyFrameInterval: 5,
		},
	})
	if err != nil {
		_ = a.Close()
		return nil, err
	}

	a.Liveness = svc

	if err := buildEnrollment(ctx, a, cfg, log, sessions, analyzer); err != nil {
		_ = a.Close()
		return nil, err
	}
	return a, nil
}

// buildEnrollment wires the gallery, the token service, and the object store.
//
// Separate from build so the enrollment path can be read as one piece, and so
// the failure of any part of it names enrollment rather than surfacing as an
// anonymous error halfway down a long function.
func buildEnrollment(
	ctx context.Context,
	a *app,
	cfg *config.Config,
	log *slog.Logger,
	sessions *postgres.SessionRepo,
	analyzer biometric.Analyzer,
) error {
	faces, err := postgres.NewFaceRepo(a.db, cfg.Enrollment.HNSWEfSearch)
	if err != nil {
		return err
	}

	tokenStore, err := postgres.NewTokenStore(a.db)
	if err != nil {
		return err
	}

	tokens, err := enrollment.NewTokenService(enrollment.TokenDeps{
		Store:   tokenStore,
		Secret:  cfg.Token.Secret.Reveal(),
		TTL:     cfg.Token.TTL,
		Entropy: rand.Reader,
	})
	if err != nil {
		return err
	}
	a.Tokens = tokens

	// Only connected when retention is on. A deployment that keeps no images
	// should not need a reachable object store to boot, and requiring one would
	// push operators towards turning retention on to make the error go away.
	var artifacts enrollment.ArtifactStore
	if cfg.Enrollment.StoreArtifacts {
		store, err := objectstore.Open(ctx, cfg.ObjectStore)
		if err != nil {
			return err
		}
		artifacts = store

		log.Warn("enrollment artifacts are being retained: face crops will be written to object storage",
			slog.String("bucket", cfg.ObjectStore.Bucket),
			slog.String("hint", "set LV_ENROLL_STORE_ARTIFACTS=false unless you need the images"))
	}

	svc, err := enrollment.NewService(enrollment.Deps{
		Faces:          faces,
		Tokens:         tokens,
		Sessions:       sessionReferences{repo: sessions},
		Analyzer:       analyzer,
		Artifacts:      artifacts,
		EncodeArtifact: encodeAlignedCrop,
		IDs:            randomFaceIDs{},
		Clock:          liveness.SystemClock{},
		Logger:         log,
		Config: enrollment.Config{
			MatchCosineMin:    cfg.Enrollment.MatchCosineMin,
			IdentityCosineMin: cfg.Liveness.IdentityCosineMin,
			SearchTopK:        cfg.Enrollment.SearchTopK,
			StoreArtifacts:    cfg.Enrollment.StoreArtifacts,
		},
	})
	if err != nil {
		return err
	}

	a.Enrollment = svc
	return nil
}

// sessionReferences adapts the liveness session repository to the little the
// enrollment path needs from it.
//
// The adapter lives here rather than in either package, because it is the only
// place allowed to know both exist. internal/liveness must not learn that
// enrollment is a thing.
type sessionReferences struct {
	repo *postgres.SessionRepo
}

func (s sessionReferences) ReferenceEmbedding(ctx context.Context, id string) (biometric.Embedding, error) {
	session, err := s.repo.Get(ctx, liveness.SessionID(id))
	if err != nil {
		return nil, err
	}

	// Only a session that actually passed may vouch for a face. A token is only
	// minted on a pass, so reaching here with anything else means the session
	// changed underneath the token — and refusing is the safe reading.
	if session.State != liveness.StatePassed {
		return nil, fmt.Errorf("session %s is %s, not %s", id, session.State, liveness.StatePassed)
	}
	return session.ReferenceEmbedding, nil
}

// encodeAlignedCrop is the artifact encoder: the 112x112 crop the embedding was
// computed from, as a JPEG.
//
// The aligned crop rather than the whole frame. It is what the model actually
// saw, it is kilobytes rather than megabytes, and it excludes whatever else
// happened to be in the room.
func encodeAlignedCrop(img image.Image, kps biometric.Keypoints) ([]byte, string, error) {
	crop, err := imaging.AlignFace(img, kps, alignedCropSize)
	if err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, crop, &jpeg.Options{Quality: 92}); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), "image/jpeg", nil
}

// alignedCropSize matches the embedder's input, since that is the image whose
// contents the stored vector actually describes.
const alignedCropSize = 112

// randomFaceIDs generates gallery identifiers.
//
// Random rather than sequential for the same reason session ids are: a
// predictable id is one somebody can guess, and here it also appears in the
// object store key.
type randomFaceIDs struct{}

func (randomFaceIDs) NewID() (enrollment.FaceID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate face id: %w", err)
	}
	return enrollment.FaceID(hex.EncodeToString(b[:])), nil
}

// buildAnalyzer returns either the real pipeline or the deterministic stub.
//
// The stub is the default so the service starts on a checkout with no models.
// It measures nothing about faces, and the log line says so on every boot.
func buildAnalyzer(cfg *config.Config, log *slog.Logger) (biometric.Analyzer, *onnx.Runtime, error) {
	if cfg.Models.Mode == config.PipelineStub {
		log.Warn("running with the stub biometric pipeline; it measures nothing about faces",
			slog.String("hint", "set LV_PIPELINE_MODE=onnx once the models are downloaded"))
		return &stub.Pipeline{}, nil, nil
	}

	rt, err := onnx.NewRuntime(cfg.Models.SharedLibrary)
	if err != nil {
		return nil, nil, err
	}

	load := func(name, file string) (*onnx.Pool, error) {
		return rt.LoadModel(onnx.ModelSpec{
			Name:           name,
			Path:           filepath.Join(cfg.Models.Dir, file),
			PoolSize:       cfg.Models.PoolSize,
			IntraOpThreads: cfg.Models.IntraOpThreads,
		})
	}

	fail := func(err error) (biometric.Analyzer, *onnx.Runtime, error) {
		_ = rt.Close()
		return nil, nil, err
	}

	detectorPool, err := load("detector", cfg.Models.Detector)
	if err != nil {
		return fail(err)
	}
	landmarkPool, err := load("landmarker", cfg.Models.Landmarker)
	if err != nil {
		return fail(err)
	}
	spoofPool, err := load("antispoof", cfg.Models.AntiSpoof)
	if err != nil {
		return fail(err)
	}
	embedPool, err := load("embedder", cfg.Models.Embedder)
	if err != nil {
		return fail(err)
	}

	detector, err := onnx.NewSCRFD(detectorPool, onnx.SCRFDOptions{
		InputSize: cfg.Models.DetectorInputSize,
		MinScore:  cfg.Liveness.MinDetectionScore,
		NMSIoU:    cfg.Models.DetectorNMSIoU,
	})
	if err != nil {
		return fail(err)
	}
	landmarker, err := onnx.NewLandmarker2d106(landmarkPool)
	if err != nil {
		return fail(err)
	}
	antiSpoofer, err := onnx.NewAntiSpoofMiniFASNet(spoofPool)
	if err != nil {
		return fail(err)
	}
	embedder, err := onnx.NewEmbedderArcFace(embedPool)
	if err != nil {
		return fail(err)
	}

	gate := imaging.Gate{
		MinLaplacianVariance: cfg.Imaging.MinLaplacianVar,
		MinBrightness:        cfg.Imaging.MinBrightness,
		MaxBrightness:        cfg.Imaging.MaxBrightness,
		MinFaceWidth:         cfg.Imaging.MinFaceWidthPx,
	}

	pipeline, err := biometric.NewPipeline(biometric.Pipeline{
		Detector:     detector,
		Landmarker:   landmarker,
		AntiSpoofer:  antiSpoofer,
		Embedder:     embedder,
		Quality:      gate.QualityCheck,
		MinFaceWidth: cfg.Imaging.MinFaceWidthPx,
	})
	if err != nil {
		return fail(err)
	}

	log.Info("biometric pipeline loaded",
		slog.String("runtime", onnx.Version()),
		slog.String("detector", cfg.Models.Detector),
		slog.Int("detector_input", cfg.Models.DetectorInputSize),
	)
	return pipeline, rt, nil
}

// randomIDs generates session identifiers.
//
// Random rather than sequential: an identifier a client can guess is one they
// can use to look at somebody else's session.
type randomIDs struct{}

func (randomIDs) NewID() (liveness.SessionID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	return liveness.SessionID(hex.EncodeToString(b[:])), nil
}
