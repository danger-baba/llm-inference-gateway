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
	docker compose -f deploy/docker-compose.yml run --rm migrate up

migrate-down:
	docker compose -f deploy/docker-compose.yml run --rm migrate down 1

seed:
	docker compose -f deploy/docker-compose.yml exec -T postgres psql -U gateway -d gateway -v ON_ERROR_STOP=1 -c "$$(cat deploy/seed.sql)"
	@echo ""
	@echo "Demo org/team ready. Issue a key with:"
	@echo "  curl -s -X POST localhost:8080/admin/keys -d '{\"team_id\":\"00000000-0000-0000-0000-000000000002\",\"label\":\"demo\"}'"
