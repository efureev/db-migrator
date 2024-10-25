package db

import (
	"github.com/efureev/db-migrator/src/config"
	"log"
)

// Wipe all structure & data in db
func Wipe() {
	config.Check()
	log.Println(`wiping...`)

	err := migrateManager().Drop()

	failError(err, "wiping error")

	log.Println(`db is clear!`)
}
