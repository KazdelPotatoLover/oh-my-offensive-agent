package config

import "os"

type Config struct {
	DatabaseURL    string
	Port           string
	CORSOrigins    string
	MigrationsPath string
}

func Load() *Config {
	c := &Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		Port:           os.Getenv("PORT"),
		CORSOrigins:    os.Getenv("CORS_ORIGINS"),
		MigrationsPath: os.Getenv("MIGRATIONS_PATH"),
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.CORSOrigins == "" {
		c.CORSOrigins = "http://localhost:3000"
	}
	if c.MigrationsPath == "" {
		c.MigrationsPath = "./migrations"
	}
	return c
}
