LDFLAGS := -s -w
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64

.PHONY: build test build-all clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/serbu .

test:
	go test ./...

build-all:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = windows ] && ext=".exe"; \
		echo "→ dist/serbu-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/serbu-$$os-$$arch$$ext . || exit 1; \
	done

clean:
	rm -rf bin dist
