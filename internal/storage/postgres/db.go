// Package postgres holds the PostgreSQL adapters.
//
// It is the only package that knows SQL. The domain packages depend on the
// repository interfaces they declare themselves, so a query changing shape
// never reaches them.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/ziad/liveness-verifier/internal/config"
	"github.com/ziad/liveness-verifier/migrations"
)

// migrationDir is the path inside the embedded filesystem. The files sit at its
// root, so goose is pointed at ".".
const migrationDir = "."

// DB is a connection pool.
type DB struct {
	Pool *pgxpool.Pool
	dsn  string
}

// Open connects and verifies the connection.
//
// The ping is not optional: a pool constructs lazily, so without it the first
// failure would surface on a user's request rather than at start-up.
func Open(ctx context.Context, cfg config.Database) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		return nil, fmt.Errorf("postgres: parse connection string: %w", err)
	}

	poolCfg.MaxConns = int32(cfg.MaxConns)
	poolCfg.MinConns = int32(cfg.MinConns)
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	return &DB{Pool: pool, dsn: cfg.URL.Reveal()}, nil
}

// Close releases every connection.
func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
}

// Ping reports whether the database is reachable.
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.Pool == nil {
		return errors.New("postgres: no pool")
	}
	return db.Pool.Ping(ctx)
}

// Migrate applies every pending migration.
//
// It is called from an explicit command rather than at start-up. Migrating on
// boot means every replica races to alter the schema at the same moment, and a
// rollback becomes a redeploy.
func Migrate(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		return goose.UpContext(ctx, db, migrationDir)
	})
}

// MigrateDown rolls back the most recent migration.
func MigrateDown(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		return goose.DownContext(ctx, db, migrationDir)
	})
}

// MigrationStatus writes the applied and pending migrations to the log goose is
// configured with.
func MigrationStatus(ctx context.Context, dsn string) error {
	return withGoose(ctx, dsn, func(db *sql.DB) error {
		return goose.StatusContext(ctx, db, migrationDir)
	})
}

// MigrationVersion reports the schema version currently applied.
func MigrationVersion(ctx context.Context, dsn string) (int64, error) {
	var version int64
	err := withGoose(ctx, dsn, func(db *sql.DB) error {
		v, err := goose.GetDBVersionContext(ctx, db)
		version = v
		return err
	})
	return version, err
}

// withGoose opens a plain database/sql handle, which is what goose speaks, and
// hands it to fn.
func withGoose(ctx context.Context, dsn string, fn func(*sql.DB) error) error {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("postgres: parse connection string: %w", err)
	}

	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer func() { _ = db.Close() }()

	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("postgres: connect for migration: %w", err)
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("postgres: set migration dialect: %w", err)
	}

	return fn(db)
}

// VerifySchema reports whether the database is at the version this binary
// expects.
//
// Checked at readiness rather than only at boot because the two can drift apart
// afterwards: a rolling deployment runs old and new binaries against one
// database, and an instance whose schema moved underneath it will fail on the
// first request touching a new column — after it is already taking traffic.
func VerifySchema(ctx context.Context, db *DB) error {
	if db == nil || db.Pool == nil {
		return errors.New("postgres: no pool")
	}

	var applied int64
	err := db.Pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&applied)
	if err != nil {
		return fmt.Errorf("postgres: read schema version: %w", err)
	}

	if applied < ExpectedSchemaVersion {
		return fmt.Errorf("postgres: schema is at version %d, this binary needs %d",
			applied, ExpectedSchemaVersion)
	}
	return nil
}

// ExpectedSchemaVersion is the newest migration this binary knows about.
//
// Bumped by hand when a migration is added, which is deliberate: deriving it
// from the embedded files would make it drift silently whenever somebody
// added a migration without thinking about compatibility, and thinking about
// compatibility is the entire point of the number.
const ExpectedSchemaVersion = 6
