package db

import (
	"github.com/efureev/db-migrator/src/config"
	"github.com/golang-migrate/migrate/v4"

	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

type migrateManagerConfig struct {
	MigrationsPath string
}

func migrateManager() *migrate.Migrate {
	return migrateManagerCustomConfig(defaultMigrateConfig())
}

func migrateManagerCustomConfig(mConfig *migrateManagerConfig) *migrate.Migrate {
	dbStr := connectionStr(config.Get().Database)

	m, err := migrate.New("file://"+mConfig.MigrationsPath, dbStr)

	failError(err, "Ошибка создания инстанса migrate PG")

	return m
}

func defaultMigrateConfig() *migrateManagerConfig {
	return &migrateManagerConfig{
		MigrationsPath: config.Get().Migrations.Path,
	}
}
