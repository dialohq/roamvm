.PHONY: build test generate image fmt fmt-check
IMAGE ?= roamvm:dev
GOFUMPT = go run mvdan.cc/gofumpt@v0.12.0
RUFF ?= ruff

fmt:
	$(GOFUMPT) -w api cmd internal test
	$(RUFF) check --select I --fix test
	$(RUFF) format test

fmt-check:
	@files="$$($(GOFUMPT) -l api cmd internal test)" && test -z "$$files" || { echo "Run make fmt to format Go sources"; exit 1; }
	$(RUFF) check --select F,I test
	$(RUFF) format --check test

build:
	CGO_ENABLED=0 go build -trimpath -o bin/roamvm ./cmd/roamvm
	CGO_ENABLED=0 go build -trimpath -o bin/lab-tool ./test/lab

test:
	go vet ./...
	go test -race ./...

generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 object paths=./api/... crd output:crd:artifacts:config=config/crd

image:
	docker build -t $(IMAGE) .
