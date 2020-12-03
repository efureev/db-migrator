package db

import (
	"log"
)

// MigrateDown migrates down
func MigrateDown() {
	log.Println(`migrating down...`)

	failError(migrateManager().Down(), "Ошибка отката миграции")

	log.Println(`migrated down done`)
}
