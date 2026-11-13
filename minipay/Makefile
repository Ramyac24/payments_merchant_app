DB_URL      ?= postgres://postgres:postgres@localhost:5432/minipay?sslmode=disable
TEST_DB_URL ?= postgres://postgres:postgres@localhost:5432/minipay_test?sslmode=disable

.PHONY: up down db test run fake merchant lint

up:            ## start Postgres + API (fake Stripe) in Docker
	docker compose up --build -d

down:
	docker compose down

db:            ## start only Postgres and create the test database
	docker compose up -d postgres
	@sleep 3
	docker compose exec postgres psql -U postgres -c "CREATE DATABASE minipay_test" || true

test:          ## unit + integration tests (needs `make db`)
	MINIPAY_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race -count=1 -p 1 ./...

fake:          ## run the API locally against fake Stripe
	DATABASE_URL="$(DB_URL)" MINIPAY_FAKE_STRIPE=1 go run ./cmd/api

run:           ## run the API locally against Stripe test mode (set STRIPE_* env vars)
	DATABASE_URL="$(DB_URL)" go run ./cmd/api

merchant:      ## create a merchant and print its API key
	DATABASE_URL="$(DB_URL)" go run ./cmd/minipayctl create-merchant -name "Demo"

lint:
	go vet ./...
