.PHONY: up down test test-race lint migrate

up:
	docker compose -f deploy/docker-compose.yml up -d --build

down:
	docker compose -f deploy/docker-compose.yml down -v

test:
	go test ./...

test-race:
	go test ./... -race

lint:
	golangci-lint run

migrate:
	@echo "no migrations yet -- schema and this target land in Phase 4"
