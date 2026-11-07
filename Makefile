ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: run build test test-integration check migrate user seed

run:
	go run ./cmd/api

build:
	go build -o bin/navo-api ./cmd/api
	go build -o bin/navo-migrate ./cmd/migrate
	go build -o bin/navo-user ./cmd/navo-user

test:
	go test -race ./...

# Needs TEST_DATABASE_URL (a disposable database whose name contains "test"); never DATABASE_URL.
test-integration:
ifndef TEST_DATABASE_URL
	$(error TEST_DATABASE_URL is not set)
endif
	go test -tags=integration -race -count=1 ./...

check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...
	go vet -tags=integration ./...
	go test -race ./...

migrate:
ifndef DATABASE_URL
	$(error DATABASE_URL is not set)
endif
	go run ./cmd/migrate

# Empty *dev*/*demo* database only: NAVO_SEED_PASSWORD=... make seed
seed:
	go run ./cmd/navo-seed

# NAVO_USER_PASSWORD=... make user EMAIL=admin@congty.vn NAME="Quản trị" ROLES=admin
user:
	go run ./cmd/navo-user -email "$(EMAIL)" -name "$(NAME)" -roles "$(ROLES)" -grants "$(GRANTS)"
