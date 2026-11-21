.PHONY: fmt test vet installer-check verify demo codex-smoke trae-smoke antigravity-smoke build-tools build-relayctl codex-capability-smoke server-up server-down db-up db-down postgres-test clean

fmt:
	GOCACHE=/private/tmp/relay-go-cache go fmt ./...

test:
	GOCACHE=/private/tmp/relay-go-cache go test ./...

vet:
	GOCACHE=/private/tmp/relay-go-cache go vet ./...

installer-check:
	sh -n ./install.sh
	sh ./scripts/test-install-service.sh

verify: fmt vet test installer-check

demo:
	GOCACHE=/private/tmp/relay-go-cache go run ./examples/multica

codex-smoke:
	GOCACHE=/private/tmp/relay-go-cache go run ./cmd/relay-codex-smoke

trae-smoke:
	GOCACHE=/private/tmp/relay-go-cache go run ./cmd/relay-trae-smoke

antigravity-smoke:
	RELAY_ANTIGRAVITY_SMOKE=1 GOCACHE=/private/tmp/relay-go-cache go test ./runtime/antigravity -run TestLocalCLI -count=1 -v

build-tools:
	mkdir -p bin
	GOCACHE=/private/tmp/relay-go-cache go build -o bin/relay-tool ./cmd/relay-tool
	GOCACHE=/private/tmp/relay-go-cache go build -o bin/relay-example-capability ./examples/capability-cli

build-relayctl:
	mkdir -p bin
	GOCACHE=/private/tmp/relay-go-cache go build -o bin/relayctl ./cmd/relayctl

codex-capability-smoke: build-tools
	GOCACHE=/private/tmp/relay-go-cache go run ./cmd/relay-codex-capability-smoke -tool-dir ./bin -binding ./bin/relay-example-capability

server-up:
	docker compose up -d --build --wait

server-down:
	docker compose down

db-up:
	POSTGRES_PASSWORD=relay RELAY_HOST_TOKEN=local-host RELAY_NODE_TOKEN=local-node docker compose up -d --wait postgres

db-down:
	POSTGRES_PASSWORD=relay RELAY_HOST_TOKEN=local-host RELAY_NODE_TOKEN=local-node docker compose stop postgres

postgres-test:
	RELAY_TEST_DATABASE_URL='postgres://relay:relay@127.0.0.1:55432/relay?sslmode=disable' GOCACHE=/private/tmp/relay-go-cache go test ./controlplane/postgres -count=1 -v

clean:
	rm -f bin/relay-tool bin/relay-example-capability bin/relayctl
