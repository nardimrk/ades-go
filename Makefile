TEMPL := go run github.com/a-h/templ/cmd/templ@v0.3.1020

.PHONY: generate build run test docker

generate:
	$(TEMPL) generate

build: generate
	CGO_ENABLED=0 go build -o bin/adesgo ./cmd/adesgo

run: generate
	go run ./cmd/adesgo

test:
	go test ./...

docker:
	docker compose up -d --build
