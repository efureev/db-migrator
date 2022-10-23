package db

import (
	"os"
	"testing"

	"github.com/efureev/db-migrator/src/config"
)

func InitConfigForTest() {
	initConfigForEnvironment(`test`)
}
func initConfigForEnvironment(env string) {
	os.Setenv(`MGTR_APP_ENVIRONMENT`, env)
	os.Setenv(`MGTR_APP_CONFIG_PATH`, `../..`)
	config.Init()
}

func Test_Init(t *testing.T) {
	InitConfigForTest()

	if config.Get().Migrations.Path != `./migrations` {
		t.Fatalf("`%s` should be `%s`", `Migrations.Path`, `./migrations`)
	}
}
