.PHONY: build fixture test update

build:
	go build -o bin/webtool ./cmd/webtool

# A test-only build that may fetch from one local fixture server. See the
# README; never point relay's real tools at it.
fixture:
	go build -tags fixture -o bin/webtool-fixture ./cmd/webtool

# Both builds' tests: the release build's and the fixture build's.
test:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go vet -tags fixture ./...
	go test -race ./...
	go test -race -tags fixture ./...

# Keep dependencies current.
update:
	go get -u ./...
	go mod tidy
	$(MAKE) test
