.PHONY: test test-race test-internal-race benchmark docker-test docker-build slate

TEST_PACKAGES ?= ./...

test:
	go test -mod=mod ./...

test-race:
	go test -mod=mod -race ./...

test-internal-race:
	go test -mod=mod -race $(TEST_PACKAGES)

benchmark:
	go test -mod=mod ./internal/storage ./internal/relay -run '^$$' -bench . -benchmem -benchtime=$${BENCHTIME:-1s}

docker-test:
	docker build --target test --build-arg TEST_PACKAGES="$(TEST_PACKAGES)" -t tinyrelay-test:local .

docker-build:
	docker build --target runtime -t tinyrelay:local .

slate:
	./scripts/test-slate.sh
