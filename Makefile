.PHONY: generate generate-check diagrams fmt test test-race vet lint vuln build compose-config up down e2e check

generate:
	go generate ./internal/generated

generate-check: generate
	git diff --exit-code -- internal/generated

diagrams:
	plantuml -checkonly -charset UTF-8 docs/diagrams/*.puml
	plantuml -tsvg -charset UTF-8 -o generated docs/diagrams/*.puml

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint:
	go tool golangci-lint run ./...

vuln:
	go tool govulncheck ./...

build:
	go build ./cmd/...

compose-config:
	docker compose config --quiet

up:
	docker compose up --build --wait

down:
	docker compose down --volumes --remove-orphans

e2e:
	./tests/e2e.sh

check: generate-check diagrams fmt test vet lint build compose-config
