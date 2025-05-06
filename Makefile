build:
		@go build -o bin/dfs

run: build
		@./bin/dfs

test:
		@go test -v ./...