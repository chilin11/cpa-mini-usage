.PHONY: build test clean
build:
	CGO_ENABLED=1 go build -buildmode=c-shared -o dist/cpa-mini-usage.so .
	rm -f dist/cpa-mini-usage.h
test:
	go test ./...
clean:
	rm -f dist/*.so dist/*.h
