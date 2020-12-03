package db

import (
	"log"
)

// Version shows version of migrations
func Version() {
	v, dirty, err := migrateManager().Version()

	failError(err, "wiping error")

	if dirty {
		log.Printf("%v (dirty)\n", v)
	} else {
		log.Println(v)
	}
}
