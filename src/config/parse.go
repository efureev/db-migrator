package config

import (
	"fmt"
	"github.com/efureev/db-migrator/src/utils"
	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"
	"log"
	"os"
)

var cfg = Config{}

func GetConfig(path string) Config {
	parseConfig(path)

	envconfig.MustProcess("DB", &(cfg.Database))
	envconfig.MustProcess("MIGRATION", &(cfg.Migrations))

	fillDefault(&cfg)

	return cfg
}

func parseConfig(path string) {
	if !utils.IsExistPath(path) {
		return
	}

	f, err := os.Open(path)
	if err != nil {
		processError(err)
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			log.Fatalln(err)
		}
	}(f)

	decoder := yaml.NewDecoder(f)
	err = decoder.Decode(&cfg)
	if err != nil {
		processError(err)
	}
}

func fillDefault(c *Config) {
	if c.Migrations.Dir == `` {
		c.Migrations.Dir = `/migrations`
	}

	if c.Database.Port == 0 {
		c.Database.Port = 5432
	}

	if c.Database.Host == `` {
		c.Database.Host = `localhost`
	}
}

func Check() {
	CheckMigrations()

	var result []string

	if cfg.Database.Name == `` {
		result = append(result, fmt.Sprintln(`Database Name can not be empty`))
	}
	if cfg.Database.User == `` {
		result = append(result, fmt.Sprintln(`Database User can not be empty`))
	}
	if cfg.Database.Host == `` {
		result = append(result, fmt.Sprintln(`Database Host can not be empty`))
	}
	if cfg.Database.Port == 0 {
		result = append(result, fmt.Sprintln(`Database Port can not be empty`))
	}
	if cfg.Migrations.Dir == `` {
		result = append(result, fmt.Sprintln(`Migrations Directory should be defined`))
	}

	if len(result) == 0 {
		return
	}

	var std = log.New(os.Stderr, "", log.LstdFlags)
	for _, err := range result {
		_ = std.Output(2, err)
	}

	os.Exit(1)
}

func CheckMigrations() {
	if !utils.IsExistPath(cfg.Migrations.Dir) {
		log.Fatalln(`Migration Folder does not exist: ` + cfg.Migrations.Dir)
	}
}
