module fms/backend

go 1.22

toolchain go1.22.12

require (
	github.com/go-chi/chi/v5 v5.3.0
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/golang-migrate/migrate/v4 v4.18.1
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.11.0
	github.com/pquerna/otp v1.4.0
	github.com/prometheus/client_golang v1.20.5
	github.com/rabbitmq/amqp091-go v1.13.0
	github.com/redis/go-redis/v9 v9.8.0
	github.com/robfig/cron/v3 v3.0.1
	github.com/shopspring/decimal v1.4.0
	github.com/stretchr/testify v1.10.0
	go.opentelemetry.io/otel v1.35.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.35.0
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.35.0
	go.opentelemetry.io/otel/sdk v1.35.0
	go.opentelemetry.io/otel/trace v1.35.0
	golang.org/x/crypto v0.34.0
)