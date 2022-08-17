package config

import (
	"fmt"
	"log"
	"os"

	"github.com/spf13/cast"
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
	Test     string
}

var c = Config{}

func Init() {
	viper.SetEnvPrefix("MGTR")
	viper.AutomaticEnv()
	viper.KeyDelimiter(`_`)

	configName := "config"
	env := os.Getenv(`MGTR_APP_ENVIRONMENT`)

	if env != `` && env != `production` {
		configName += `.` + env
	}

	viper.SetConfigName(configName) // name of config file (without extension)
	viper.AddConfigPath(".")        // optionally look for config in the working directory

	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Printf("fatal error config file: %s\n", err)
			return
		}
		fmt.Println(err)
	} else {
		err = viper.Unmarshal(&c)
		if err != nil {
			log.Fatalf("unable to decode into struct, %v", err)
		}

	}

	// @todo переделать
	if v := viper.Get(`database_host`); v != nil {
		c.Database.Host = v.(string)
	}
	if v := viper.Get(`database_user`); v != nil {
		c.Database.User = v.(string)
	}
	if v := viper.Get(`database_name`); v != nil {
		c.Database.Name = v.(string)
	}
	if v := viper.Get(`database_password`); v != nil {
		c.Database.Password = v.(string)
	}
	if v := viper.Get(`database_migrationspath`); v != nil {
		c.Database.MigrationsPath = v.(string)
	}
	if v := viper.Get(`database_port`); v != nil {
		c.Database.Port = cast.ToInt(v)
	}

	if c.Database.MigrationsPath == `` {
		c.Database.MigrationsPath = `./migrations`
	}
}

func Get() *Config {
	return &c
}
