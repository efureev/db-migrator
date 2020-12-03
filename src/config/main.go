package config

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/viper"
)

type Database struct {
	User           string
	Password       string
	Host           string
	Port           int
	Name           string
	MigrationsPath string
}

type Config struct {
	Database Database
}

var c = Config{}

func Init() {
	viper.SetEnvPrefix("MGTR")
	configName := "config"
	env := os.Getenv(`MGTR_APP_ENVIRONMENT`)

	if env != `` && env != `production` {
		configName += `.` + env
	}

	viper.SetConfigName(configName) // name of config file (without extension)
	viper.AddConfigPath(".")        // optionally look for config in the working directory

	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {
		panic(fmt.Errorf("fatal error config file: %s", err))
	}

	err = viper.Unmarshal(&c)
	if err != nil {
		log.Fatalf("unable to decode into struct, %v", err)
	}
}

func Get() *Config {
	return &c
}
