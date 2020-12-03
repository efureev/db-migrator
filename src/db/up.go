package db

import (
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
)

// MigrateUp migrates up
func MigrateUp() {
	log.Println(`migrating up...`)

	err := migrateManager().Up()

	switch err {
	case migrate.ErrNoChange:
		log.Println(`[DB] ` + err.Error())
		return

	case migrate.ErrNilVersion:
		log.Println(`[DB] ` + err.Error())
		return

	case err.(*os.PathError):
		log.Println(`[DB] ` + err.Error())
		return
	}

	failError(err, "Ошибка наката миграций")

	log.Println(`migrated up done`)
}
