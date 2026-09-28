.PHONY: build test vet fmt lint docker docker-gasless run run-gasless migrate e2e e2e-live burst sweep channels

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

docker:
	docker build -t latch-relayer .

docker-gasless:
	docker build -f Dockerfile.gasless -t latch-gasless .

# Deposit bridge (reads .env)
run:
	go run ./cmd/serve

# Gasless sponsorship service (reads gasless.env)
run-gasless:
	go run ./cmd/gasless

migrate:
	go run ./cmd/serve

e2e:
	go run ./scripts/e2e_test/main.go

# Live testnet E2E through latch-api (reads e2e.env, then .env):
# make e2e-live RUN=happy,nomemo TIMEOUT=5m
e2e-live:
	go run ./scripts/e2e_live $(if $(RUN),-run $(RUN)) $(if $(TIMEOUT),-timeout $(TIMEOUT))

# Burst deposits straight at the relayer and time the forwards:
# make burst N=50 C=CB... (CSV=burst.csv to keep per-deposit rows)
burst:
	go run ./scripts/burst $(if $(N),-n $(N)) $(if $(C),-c $(C)) $(if $(AMOUNT),-amount $(AMOUNT)) $(if $(CSV),-csv $(CSV))

sweep:
	go run ./scripts/sweep_test/main.go

# Create any missing gasless channel accounts: make channels N=10
# (N defaults to CHANNEL_COUNT from gasless.env). Add MERGE_TO=25 to also
# merge channels N..24 back into the funder, DRY_RUN=1 to only report.
channels:
	go run ./scripts/channels $(if $(N),-n $(N)) $(if $(MERGE_TO),-merge-to $(MERGE_TO)) $(if $(DRY_RUN),-dry-run)

# Create any missing deposit-bridge channel accounts (#48), funded by pool 1:
# make deposit-channels N=10 (N defaults to DEPOSIT_CHANNEL_COUNT from .env).
deposit-channels:
	go run ./scripts/channels -deposit $(if $(N),-n $(N)) $(if $(MERGE_TO),-merge-to $(MERGE_TO)) $(if $(DRY_RUN),-dry-run)
