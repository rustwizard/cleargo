package pg

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v4"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/rs/zerolog"
)

const (
	maxRetry = 10
	ttlRetry = 1 * time.Second
)

var zLevels = map[pgx.LogLevel]zerolog.Level{
	pgx.LogLevelDebug: zerolog.DebugLevel,
	pgx.LogLevelInfo:  zerolog.InfoLevel,
	pgx.LogLevelWarn:  zerolog.WarnLevel,
	pgx.LogLevelError: zerolog.ErrorLevel,
	pgx.LogLevelNone:  zerolog.NoLevel,
}

type Config struct {
	Host         string `mapstructure:"HOST"`
	Port         int    `mapstructure:"PORT"`
	User         string `mapstructure:"USER"`
	Password     string `mapstructure:"PASSWORD"`
	DatabaseName string `mapstructure:"NAME"`
	Schema       string `mapstructure:"SCHEME"`
	SSL          string `mapstructure:"SSL"`
	MaxPoolSize  int    `mapstructure:"POOL_SIZE"`
}

type DB struct {
	Pool *pgxpool.Pool
	log  zerolog.Logger
}

func NewDB() *DB {
	return &DB{log: zerolog.New(os.Stdout).With().Str("pkg", "postgres").Logger()}
}

func (d *DB) Connect(dbc *Config) error {
	args := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s&pool_max_conns=%d",
		dbc.User,
		dbc.Password,
		dbc.Host,
		dbc.Port,
		dbc.DatabaseName,
		dbc.SSL,
		dbc.MaxPoolSize,
	)
	poolConfig, err := pgxpool.ParseConfig(args)
	if err != nil {
		d.log.Error().Err(err).Msg("parse config")
		return err
	}

	poolConfig.BeforeAcquire = d.CheckConn
	poolConfig.ConnConfig.Logger = d
	var db *pgxpool.Pool
	retry := 1
	for retry < maxRetry {
		db, err = pgxpool.ConnectConfig(context.Background(), poolConfig)
		if err != nil {
			d.log.Error().Err(err).Int("retry", retry).
				Dur("second", ttlRetry+(1<<retry)*time.Second).Msg("")
			retry++
			time.Sleep(ttlRetry + (1<<retry)*time.Second)
			continue
		}
		break
	}

	d.Pool = db
	return err
}

func (d *DB) CheckConn(ctx context.Context, pgc *pgx.Conn) bool {
	if pgc == nil {
		return false
	}

	if err := pgc.Ping(ctx); err != nil {
		attempt := 0
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if attempt >= maxRetry {
				d.log.Info().Msg("postgres: max reconnect attempt")
				return false
			}
			attempt++

			d.log.Info().Msg("postgres: try to reconnect")

			newPgc, connErr := d.Pool.Acquire(ctx)
			if connErr != nil {
				d.log.Error().Err(err).Msg("postgres: lost connection")
				continue
			}

			pgc = newPgc.Conn()
			break
		}
	}

	return pgc != nil
}

func (d *DB) Log(ctx context.Context, level pgx.LogLevel, msg string, data map[string]interface{}) {
	lvl, _ := fromZLevel(level)
	logger := d.log.With().Fields(data).Logger()
	logger.WithLevel(lvl).Msg(msg)
}

func fromZLevel(level pgx.LogLevel) (zerolog.Level, pgx.LogLevel) {
	zlvl, found := zLevels[level]
	if found {
		return zlvl, level
	}

	return zerolog.NoLevel, pgx.LogLevelNone
}
