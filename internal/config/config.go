package config

import (
	"log/slog"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port string
	DBUrl string
}

func LoadConfig() *Config {
	err := godotenv.Load()
	if err != nil {
		slog.Warn("No .env file found, relying on system environment variables")
	}
	
	return &Config{
			Port: getEnv("PORT", "8080"), // default value 8080
			DBUrl: getEnv("DB_URL", ""),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
			return value
	}
	return fallback
}