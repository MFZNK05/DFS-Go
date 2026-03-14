build:
		@go build -o bin/hermod

run: build
		@./bin/hermod

test:
		@go test -v ./...