package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/raymondkneipp/aggregate/internal/config"
	"github.com/raymondkneipp/aggregate/migrations"
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context) (string, error) {
	cfg := config.Load()

	d, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return "", fmt.Errorf("load migrations: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", d, cfg.DatabaseURL)
	if err != nil {
		return "", fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return "", fmt.Errorf("run migrations: %w", err)
	}

	version, _, _ := m.Version()
	slog.Info("migrations complete", "version", version)
	return fmt.Sprintf("migrated to version %d", version), nil
}
