.PHONY: up down test test-race lint migrate migrate-down seed download-embedding-model mock-provider loadtest-seed-key chaos

# Fetches the OS-independent embedding assets (model + vocab) for local
# development/testing of internal/embedding and internal/cache/semantic
# outside Docker. The Dockerfile fetches its own (Linux) copies at image
# build time, independently of this target. You still need the ONNX
# Runtime shared library for your own OS -- see docs/adr/0012.
download-embedding-model:
	mkdir -p .cache/minilm
	curl -sL -o .cache/minilm/model.onnx "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/onnx/model_quantized.onnx"
	curl -sL -o .cache/minilm/vocab.txt "https://huggingface.co/Xenova/all-MiniLM-L6-v2/resolve/main/vocab.txt"
	@echo "Model assets downloaded to .cache/minilm."
	@echo "Now fetch the ONNX Runtime shared library release for your OS from:"
	@echo "  https://github.com/microsoft/onnxruntime/releases/tag/v1.28.0"
	@echo "and set ONNXRUNTIME_LIB_PATH (or EMBEDDING_MODEL_PATH/EMBEDDING_VOCAB_PATH if you moved these) accordingly."

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

# Starts the real gateway binary against deploy/loadtest.config.yaml (a
# single always-healthy mock provider, fixed 200ms latency) for
# loadtest/overhead.js and loadtest/cache.js. Needs a real Redis at
# localhost:6379 (or edit the config) and a virtual key seeded first --
# see loadtest-seed-key. Postgres is deliberately left unreachable; see
# deploy/loadtest.config.yaml's header comment and docs/adr/0015.
mock-provider:
	POSTGRES_DSN="postgres://fake:fake@127.0.0.1:1/fakedb" go run ./cmd/gateway --config deploy/loadtest.config.yaml

# Seeds a real Redis instance with a virtual-key identity at the exact
# cache shape internal/auth.CachingResolver expects, so load-test runs
# never touch Postgres for auth. Benchmarking convenience only -- see
# cmd/loadtest-seed-key's own doc comment.
loadtest-seed-key:
	go run ./cmd/loadtest-seed-key "$${REDIS_ADDR:-localhost:6379}" sk-vk-loadtest

# Kills and restarts the gateway process mid-run under steady k6 traffic
# and asserts the client-visible success rate recovers -- see
# loadtest/chaos.sh for exactly what this does and does not verify in
# this environment.
chaos:
	bash loadtest/chaos.sh
