.PHONY: build run test test-clients vet

build:
	go build -trimpath -o bin/packcove ./cmd/packcove

run:
	go run ./cmd/packcove -config packcove.yaml

test:
	go test ./...

test-clients:
	PACKCOVE_RUN_CLIENT_TESTS=1 go test -v ./internal/packcove -run '^Test(NPM|PyPI|Maven)ClientInstall$$' -count=1

vet:
	go vet ./...
