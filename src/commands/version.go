package commands

import (
	"github.com/integrii/flaggy"
)

var version = "unknown"

func getVersion() {
	flaggy.SetVersion(version)
}
