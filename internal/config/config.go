package config

import (
	"log/slog"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port      string
	DBUrl     string
	JWTSecret string
}

func LoadConfig() *Config {
	// godotenv.Load is a no-op when .env is absent; the warning is expected in
	// production environments (e.g. Railway) that inject vars at the OS level.
	if err := godotenv.Load(); err != nil {
		slog.Warn("No .env file found, relying on system environment variables")
	}

	return &Config{
		Port: getEnv("PORT", "8080"),
		// Railway injects DATABASE_URL; DB_URL is kept as a local-dev fallback.
		DBUrl:     getEnvFirstMatch("DATABASE_URL", "DB_URL"),
		JWTSecret: getEnv("JWT_SECRET", ""),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// getEnvFirstMatch returns the value of the first key that is set and non-empty.
// This lets production vars (e.g. DATABASE_URL on Railway) transparently take
// precedence over local-dev vars (e.g. DB_URL) without branching in LoadConfig.
func getEnvFirstMatch(keys ...string) string {
	for _, key := range keys {
		if value, exists := os.LookupEnv(key); exists && value != "" {
			return value
		}
	}
	return ""
}
