.PHONY: build test generate image fmt fmt-check lab-up lab-install lab-down lab-guest lab-nixos-guest lab-generation-guest lab-store integration benchmark
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
	nu --no-config-file test/runner-test.nu

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

lab-nixos-guest: build
	mkdir -p .lab
	nix build .#test-nixos-guest --out-link .lab/nixos-guest.tar.gz
	bin/roamvm image-push --plain-http --tag localhost:15001/test-nixos-guest:local --tar .lab/nixos-guest.tar.gz > .lab/nixos-guest-ref

lab-generation-guest: build
	mkdir -p .lab
	nix build .#test-generation-guest --out-link .lab/generation-guest.tar.gz
	nix build .#test-firmware-guest --out-link .lab/firmware-guest.tar.gz
	bin/roamvm image-push --plain-http --tag localhost:15001/generation-guest:local --tar .lab/generation-guest.tar.gz > .lab/generation-guest-ref
	bin/roamvm image-push --plain-http --tag localhost:15001/firmware-guest:local --tar .lab/firmware-guest.tar.gz > .lab/firmware-guest-ref

integration: libvirt-scenarios

benchmark:
	go test -tags=integration -run '^$$' -bench BenchmarkStartup -benchtime=5x -count=1 -timeout=30m -v ./test/integration

lab-down:
	kind delete cluster --name roamvm-test
	$(COMPOSE) down

.PHONY: libvirt-check libvirt-up libvirt-plan libvirt-destroy libvirt-install libvirt-fixtures libvirt-down libvirt-freeze libvirt-freeze-warm libvirt-reset libvirt-reset-cold libvirt-scenario libvirt-scenarios
libvirt-check:
	bash test/libvirt/check.sh

libvirt-up:
	bash test/libvirt/lab.sh up

libvirt-plan:
	bash test/libvirt/lab.sh plan

libvirt-destroy:
	bash test/libvirt/lab.sh destroy

libvirt-install:
	bash test/libvirt/install.sh

libvirt-fixtures:
	bash test/libvirt/lab.sh fixtures

libvirt-down:
	bash test/libvirt/lab.sh down

libvirt-freeze:
	bash test/libvirt/scenario.sh freeze

libvirt-freeze-warm:
	bash test/libvirt/scenario.sh freeze-warm

.PHONY: libvirt-export libvirt-import
libvirt-export:
	bash test/libvirt/bundle.sh export "$(BUNDLE)"

libvirt-import:
	bash test/libvirt/bundle.sh import "$(BUNDLE)"

libvirt-reset:
	bash test/libvirt/scenario.sh reset

libvirt-reset-cold:
	bash test/libvirt/scenario.sh reset-cold

libvirt-scenario:
	bash test/libvirt/scenario.sh run "$(SCENARIO)"

libvirt-scenarios:
	bash test/libvirt/scenario.sh run-all

.PHONY: test-scenario
test-scenario: libvirt-scenario
