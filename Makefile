.PHONY: build format test vet

build:
	go build -o docket ./cmd/docket

format:
	gofmt -l -w .

# The gmail library is a nested module (own go.mod), so the root ./...
# test selection does not reach it; run both module suites.
test:
	go test ./...
	cd gmail && go test ./...

vet:
	go vet ./...
	cd gmail && go vet ./...