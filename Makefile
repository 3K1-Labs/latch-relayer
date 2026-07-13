.PHONY: build test vet fmt lint docker run migrate e2e sweep

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

run:
	go run ./cmd/serve

migrate:
	go run ./cmd/serve

e2e:
	go run ./scripts/e2e_test/main.go

sweep:
	go run ./scripts/sweep_test/main.go
