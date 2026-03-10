build:
		@go build -o bin/hermond

run: build
		@./bin/hermond

test:
		@go test -v ./...