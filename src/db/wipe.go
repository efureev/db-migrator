package db

import (
	"log"
)

// Wipe all structure & data in db
func Wipe() {
	log.Println(`wiping...`)

	err := migrateManager().Drop()

	failError(err, "wiping error")

	log.Println(`db is clear!`)
}
