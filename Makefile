.PHONY: build run docker-build docker-up migrate

build:
	go build -o bin/payflow ./cmd/server

run: build
	./bin/payflow

docker-build:
	docker build -t payflow:local .

docker-up:
	docker-compose up --build

migrate:
	@echo "apply SQL migrations in ./migrations"
