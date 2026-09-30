.PHONY: build test generate image fmt fmt-check lab-up lab-install lab-down lab-guest lab-store integration benchmark
IMAGE ?= roamvm:dev
GOFUMPT = go run mvdan.cc/gofumpt@v0.12.0
COMPOSE = docker compose -f test/lab/compose.yaml

fmt:
	$(GOFUMPT) -w api cmd internal test

fmt-check:
	@files="$$($(GOFUMPT) -l api cmd internal test)" && test -z "$$files" || { echo "Run make fmt to format Go sources"; exit 1; }

build:
	CGO_ENABLED=0 go build -trimpath -o bin/roamvm ./cmd/roamvm

test:
	go vet ./...
	go test -race ./...
	go test -tags=integration ./test/integration -run '^$$'

generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 object paths=./api/... crd output:crd:artifacts:config=config/crd

image:
	docker build -t $(IMAGE) .

lab-store:
	mkdir -p .lab
	nix build .#test-store --out-link .lab/store
	docker load -i .lab/store

lab-up: build lab-store
	@test -c /dev/kvm
	kind create cluster --name roamvm-test --image kindest/node:v1.35.0 --config test/lab/kind.yaml --kubeconfig .lab/kubeconfig
	$(COMPOSE) up -d --wait
	$(COMPOSE) run --rm bucket
	$(MAKE) image lab-install IMAGE=roamvm:dev

lab-install: build
	kind load docker-image roamvm:dev --name roamvm-test
	bash test/lab/install.sh

lab-guest: build
	mkdir -p .lab
	nix build .#test-guest --out-link .lab/guest.tar.gz
	bin/roamvm image-push --plain-http --tag localhost:15001/test-guest:local --tar .lab/guest.tar.gz > .lab/guest-ref

integration:
	go test -tags=integration -race -count=1 -timeout=30m -v ./test/integration

benchmark:
	go test -tags=integration -run '^$$' -bench BenchmarkStartup -benchtime=5x -count=1 -timeout=30m ./test/integration

lab-down:
	kind delete cluster --name roamvm-test
	$(COMPOSE) down
