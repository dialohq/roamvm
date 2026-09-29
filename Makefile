.PHONY: build test generate image
IMAGE ?= roamvm:dev

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
