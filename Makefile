.PHONY: build test run release

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

run:
	go run ./cmd/server -data ./data

# Cross-compile server + fsctl into ./bin/<os>-<arch>/
release:
	for t in windows/amd64 linux/amd64 linux/arm64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w" -o bin/$$os-$$arch/ ./cmd/... ; \
	done
