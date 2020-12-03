package db

import (
	"fmt"
	"log"

	"migrator/src/config"
)

func failError(err error, msg string) {
	if err != nil {
		log.Fatalf("[DB]\t%s: %s", msg, err)
	}
}

func connectionStr(o config.Database) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		o.User,
		o.Password,
		o.Host,
		o.Port,
		o.Name,
	)
}
