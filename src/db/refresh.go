package db

import (
	"log"
)

// MigrateRefresh Refresh all migrations
func MigrateRefresh() {
	log.Println(`refreshing...`)

	failError(migrateManager().Drop(), "Ошибка удаления миграций")
	failError(migrateManager().Up(), "Ошибка наката миграций")

	log.Println(`All migrations have installed`)
}
