.PHONY: build test clean
build:
	go build -buildmode=c-shared -o dist/quota-api.dylib .
test:
	go test ./...
clean:
	rm -rf dist
