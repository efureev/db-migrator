package config

import "fmt"

func ViewConfig() {
	Get()

	fmt.Printf("%#v", Get())
}
