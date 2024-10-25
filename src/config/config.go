package config

type Migrations struct {
	Dir string `yaml:"dir" env:"DIR"`
}

type Database struct {
	User     string `yaml:"user" env:"USER"`
	Password string `yaml:"pass" env:"PASS"`
	Host     string `yaml:"host" env:"HOST"`
	Port     int    `yaml:"port" env:"POST"`
	Name     string `yaml:"name" env:"NAME"`
}

type Config struct {
	Migrations Migrations `yaml:"migrations"`
	Database   Database   `yaml:"database"`
}
