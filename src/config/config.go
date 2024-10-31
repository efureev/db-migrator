package config

type Migrations struct {
	Dir string `yaml:"dir" env:"DIR"`
}

type Database struct {
	User string `yaml:"user" env:"USER"`
	Pass string `yaml:"pass" env:"PASS"`
	Host string `yaml:"host" env:"HOST"`
	Port int    `yaml:"port" env:"PORT"`
	Name string `yaml:"name" env:"NAME"`
}

type Config struct {
	Migrations Migrations `yaml:"migrations"`
	Database   Database   `yaml:"database"`
}
