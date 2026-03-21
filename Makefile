BIN := bin/rns-email-iface
CMD := ./cmd/rns-email-iface

.PHONY: build test cover lint vet tidy clean run

build:
	go build -o $(BIN) $(CMD)

test:
	go test -race -coverprofile=coverage.out ./...

cover: test
	go tool cover -html=coverage.out

lint:
	golangci-lint run ./...

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ coverage.out

run:
	go run $(CMD)
