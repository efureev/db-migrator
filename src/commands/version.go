package commands

import (
	"github.com/integrii/flaggy"
)

var version = "unknown"
var build = "unknown"
var buildTime = "unknown"

func getVersion() {
	flaggy.SetVersion(version + ` (` + build + `). Compiling: ` + buildTime)
}
