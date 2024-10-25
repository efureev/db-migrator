package db

import (
	"github.com/efureev/db-migrator/src/config"
	"log"
)

// MigrateRefresh Refresh all migrations
func MigrateRefresh() {
	config.Check()
	log.Println(`refreshing...`)

	failError(migrateManager().Drop(), "Ошибка удаления миграций")
	failError(migrateManager().Up(), "Ошибка наката миграций")

	log.Println(`All migrations have installed`)
}
