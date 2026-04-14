package config

import (
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server   ServerConfig   `mapstructure:"server"`
	Database DatabaseConfig `mapstructure:"database"`
	Redis    RedisConfig    `mapstructure:"redis"`
	Magento  MagentoConfig  `mapstructure:"magento"`
	GraphQL  GraphQLConfig  `mapstructure:"graphql"`
	Logging  LoggingConfig  `mapstructure:"logging"`
	Stripe   StripeConfig   `mapstructure:"stripe"`
}

type ServerConfig struct {
	Port         string        `mapstructure:"port"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
}

type DatabaseConfig struct {
	Host            string        `mapstructure:"host"`
	Port            string        `mapstructure:"port"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password"`
	Name            string        `mapstructure:"name"`
	Socket          string        `mapstructure:"socket"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
}

type RedisConfig struct {
	Host     string `mapstructure:"host"`
	Port     string `mapstructure:"port"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type MagentoConfig struct {
	CryptKey      string `mapstructure:"crypt_key"`
	JWTTTLMinutes int    `mapstructure:"jwt_ttl_minutes"`
}

type StripeConfig struct {
	SecretKey      string `mapstructure:"secret_key"`
	WebhookSecret  string `mapstructure:"webhook_secret"`
	PublishableKey string `mapstructure:"publishable_key"`
}

type GraphQLConfig struct {
	ComplexityLimit int `mapstructure:"complexity_limit"`
}

type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Pretty bool   `mapstructure:"pretty"`
}

func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("./config")

	viper.SetDefault("server.port", "8084")
	viper.SetDefault("server.read_timeout", "30s")
	viper.SetDefault("server.write_timeout", "30s")
	viper.SetDefault("database.host", "localhost")
	viper.SetDefault("database.port", "3306")
	viper.SetDefault("database.user", "magento_go")
	viper.SetDefault("database.password", "magento_go")
	viper.SetDefault("database.name", "magento248")
	viper.SetDefault("database.max_open_conns", 25)
	viper.SetDefault("database.max_idle_conns", 10)
	viper.SetDefault("database.conn_max_lifetime", "5m")
	viper.SetDefault("database.conn_max_idle_time", "1m")
	viper.SetDefault("redis.host", "127.0.0.1")
	viper.SetDefault("redis.port", "6379")
	viper.SetDefault("magento.jwt_ttl_minutes", 60)
	viper.SetDefault("graphql.complexity_limit", 1000)
	viper.SetDefault("logging.level", "info")

	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	viper.BindEnv("database.host", "DB_HOST")
	viper.BindEnv("database.port", "DB_PORT")
	viper.BindEnv("database.user", "DB_USER")
	viper.BindEnv("database.password", "DB_PASSWORD")
	viper.BindEnv("database.name", "DB_NAME")
	viper.BindEnv("database.socket", "DB_SOCKET")
	viper.BindEnv("redis.host", "REDIS_HOST")
	viper.BindEnv("server.port", "SERVER_PORT")
	viper.BindEnv("magento.crypt_key", "MAGENTO_CRYPT_KEY")
	viper.BindEnv("logging.level", "LOG_LEVEL")
	viper.BindEnv("logging.pretty", "LOG_PRETTY")
	viper.BindEnv("stripe.secret_key", "STRIPE_SECRET_KEY")
	viper.BindEnv("stripe.webhook_secret", "STRIPE_WEBHOOK_SECRET")
	viper.BindEnv("stripe.publishable_key", "STRIPE_PUBLISHABLE_KEY")

	_ = viper.ReadInConfig()

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
