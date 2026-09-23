.PHONY: build test vet fmt lint docker docker-gasless run run-gasless migrate e2e sweep channels

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

sweep:
	go run ./scripts/sweep_test/main.go

# Create any missing gasless channel accounts: make channels N=10
# (N defaults to CHANNEL_COUNT from gasless.env). Add MERGE_TO=25 to also
# merge channels N..24 back into the funder, DRY_RUN=1 to only report.
channels:
	go run ./scripts/channels $(if $(N),-n $(N)) $(if $(MERGE_TO),-merge-to $(MERGE_TO)) $(if $(DRY_RUN),-dry-run)
