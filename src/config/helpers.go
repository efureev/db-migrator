package config

import (
	"fmt"
	"os"
)

func processError(err error) {
	fmt.Println(err)
	os.Exit(2)
}
