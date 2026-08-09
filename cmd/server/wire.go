package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/ziad/liveness-verifier/internal/biometric"
	"github.com/ziad/liveness-verifier/internal/biometric/onnx"
	"github.com/ziad/liveness-verifier/internal/biometric/stub"
	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/internal/imaging"
	"github.com/ziad/liveness-verifier/internal/liveness"
	"github.com/ziad/liveness-verifier/internal/storage/postgres"
)

// app holds everything that has to be shut down.
type app struct {
	db      *postgres.DB
	runtime *onnx.Runtime

	Liveness *liveness.Service
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
			EARBlink:       cfg.Liveness.EARBlink,
			EAROpen:        cfg.Liveness.EAROpen,
			BlinkMinFrames: cfg.Liveness.BlinkMinFrames,
			YawTurnDeg:     cfg.Liveness.YawTurnDeg,
			PitchNodDeg:    cfg.Liveness.PitchNodDeg,
			MARMouthOpen:   cfg.Liveness.MARMouthOpen,
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
	return a, nil
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
