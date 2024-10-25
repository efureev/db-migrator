package db

import (
	"github.com/efureev/db-migrator/src/config"
	"log"
)

// MigrateDown migrates down
func MigrateDown() {
	config.Check()
	log.Println(`migrating down...`)

	failError(migrateManager().Down(), "Ошибка отката миграции")

	log.Println(`migrated down done`)
}
