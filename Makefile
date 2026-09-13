.PHONY: build build-tiny build-tinyrelay build-tinygit build-tinyclient test test-race test-internal-race benchmark verify web-test docker-test docker-build linux-test

TEST_PACKAGES ?= ./...

build: build-tiny build-tinyrelay build-tinygit build-tinyclient

build-tiny:
	go build -mod=readonly -o bin/tiny ./cmd/tiny

build-tinyrelay:
	go build -mod=readonly -o bin/tinyrelay ./cmd/tinyrelay

build-tinygit:
	go build -mod=readonly -o bin/tinygit ./cmd/tinygit

build-tinyclient:
	go build -mod=readonly -o bin/tinyclient ./cmd/tinyclient

test:
	go test -mod=readonly ./...

test-race:
	go test -mod=readonly -race ./...

test-internal-race:
	go test -mod=readonly -race $(TEST_PACKAGES)

benchmark:
	go test -mod=readonly ./internal/storage ./internal/relay -run '^$$' -bench . -benchmem -benchtime=$${BENCHTIME:-1s}

verify:
	go test -mod=readonly ./...
	go vet -mod=readonly ./...
	npm run verify:bundles
	npm run test:js

web-test:
	npm run verify:bundles
	npm run test:js

docker-test:
	docker build --target test --build-arg TEST_PACKAGES="$(TEST_PACKAGES)" -t tinyrelay-test:local .

docker-build:
	docker build --target runtime -t tinyrelay:local .

linux-test:
	./scripts/test-linux.sh
