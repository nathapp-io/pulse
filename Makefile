.PHONY: build test race lint vet fmt tidy docker-prober docker-viewer clean

GO      ?= go
BIN     := bin
TAGS    :=
LDFLAGS := -s -w

build:
	$(GO) build $(TAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/pulse-prober ./cmd/prober
	$(GO) build $(TAGS) -ldflags '$(LDFLAGS)' -o $(BIN)/pulse-viewer ./cmd/viewer

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

lint: vet

tidy:
	$(GO) mod tidy

# static, non-root, CGO-free (modernc.org/sqlite keeps CGO off)
docker-prober:
	docker buildx build --platform linux/amd64,linux/arm64 -f deploy/prober.Dockerfile -t pulse-prober:dev .

docker-viewer:
	docker buildx build --platform linux/amd64,linux/arm64 -f deploy/viewer.Dockerfile -t pulse-viewer:dev .

clean:
	rm -rf $(BIN)
