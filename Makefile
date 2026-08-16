.PHONY: build format test vet

build:
	go build -o docket ./cmd/docket

format:
	gofmt -l -w .

test:
	go test ./...

vet:
	go vet ./...
