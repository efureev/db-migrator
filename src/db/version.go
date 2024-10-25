package db

import (
	"github.com/efureev/db-migrator/src/config"
	"log"
)

// Version shows version of migrations
func Version() {
	config.Check()
	v, dirty, err := migrateManager().Version()

	failError(err, "wiping error")

	if dirty {
		log.Printf("%v (dirty)\n", v)
	} else {
		log.Println(v)
	}
}
