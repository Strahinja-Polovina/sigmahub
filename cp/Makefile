DATABASE_URL ?= postgres://sigmahub:sigmahub@localhost:5433/sigmahub_cp

.PHONY: dev db-up db-down run build test vet

db-up:
	docker compose -f docker-compose.dev.yml up -d --wait

db-down:
	docker compose -f docker-compose.dev.yml down

run:
	CP_DATABASE_URL=$(DATABASE_URL) go run ./cmd/sigmahub-cp

dev: db-up run

build:
	go build -o bin/sigmahub-cp ./cmd/sigmahub-cp

test:
	go test ./...

vet:
	go vet ./...
