package db

import (
	"github.com/efureev/db-migrator/src/config"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

// MigrateUp migrates up
func MigrateUp() {
	config.Check()
	log.Println(`migrating up...`)

	err := migrateManager().Up()

	if err == nil {
		log.Println(`migrated up done`)
		return
	}

	if e, ok := err.(migrate.ErrDirty); ok {
		log.Println(`[DB] ` + e.Error())
		return
	}

	if e, ok := err.(migrate.ErrShortLimit); ok {
		log.Println(`[DB] ` + e.Error())
		return
	}

	if e, ok := err.(database.Error); ok {
		log.Println(`[DB] ` + e.Error())
		return
	}

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

	log.Fatalf("[DB]\t%s: %s", "Ошибка наката миграций", err)
}
