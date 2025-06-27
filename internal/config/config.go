package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server ServerConfig
	Redis  RedisConfig
	AWS    AWSConfig
	App    AppConfig
}

type ServerConfig struct {
	Port               string
	Host               string
	ReadTimeout        time.Duration
	WriteTimeout       time.Duration
	ShutdownTimeout    time.Duration
	MaxConnections     int
	ConnectionsPerTask int
}

type RedisConfig struct {
	Addr         string
	Password     string
	DB           int
	PoolSize     int
	MinIdleConns int
}

type AWSConfig struct {
	Region          string
	DynamoDBTable   string
	CloudWatchGroup string
}

type AppConfig struct {
	Environment   string
	LogLevel      string
	EnableMetrics bool
	RoomTTL       time.Duration
}

func Load() *Config {
	return &Config{
		Server: ServerConfig{
			Port:               getEnv("SERVER_PORT", "8080"),
			Host:               getEnv("SERVER_HOST", "0.0.0.0"),
			ReadTimeout:        getDurationEnv("SERVER_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:       getDurationEnv("SERVER_WRITE_TIMEOUT", 30*time.Second),
			ShutdownTimeout:    getDurationEnv("SERVER_SHUTDOWN_TIMEOUT", 30*time.Second),
			MaxConnections:     getIntEnv("MAX_CONNECTIONS", 240000),
			ConnectionsPerTask: getIntEnv("CONNECTIONS_PER_TASK", 50000),
		},
		Redis: RedisConfig{
			Addr:         getEnv("REDIS_ADDR", "localhost:6379"),
			Password:     getEnv("REDIS_PASSWORD", ""),
			DB:           getIntEnv("REDIS_DB", 0),
			PoolSize:     getIntEnv("REDIS_POOL_SIZE", 100),
			MinIdleConns: getIntEnv("REDIS_MIN_IDLE_CONNS", 10),
		},
		AWS: AWSConfig{
			Region:          getEnv("AWS_REGION", "us-east-1"),
			DynamoDBTable:   getEnv("DYNAMODB_TABLE", "fire-tv-rooms"),
			CloudWatchGroup: getEnv("CLOUDWATCH_LOG_GROUP", "/aws/ecs/fire-tv-rooms"),
		},
		App: AppConfig{
			Environment:   getEnv("ENVIRONMENT", "development"),
			LogLevel:      getEnv("LOG_LEVEL", "info"),
			EnableMetrics: getBoolEnv("ENABLE_METRICS", true),
			RoomTTL:       getDurationEnv("ROOM_TTL", 24*time.Hour),
		},
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intValue, err := strconv.Atoi(value); err == nil {
			return intValue
		}
	}
	return defaultValue
}

func getBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return defaultValue
}
