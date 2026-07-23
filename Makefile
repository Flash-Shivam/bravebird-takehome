SHELL := /bin/bash
TF := terraform -chdir=terraform
MY_IP = $(shell curl -s https://checkip.amazonaws.com)
TF_VARS = -var "allowed_cidr=$(MY_IP)/32"

.PHONY: build test build-reaper ecr-login push-images deploy destroy api-url token run-local loadtest agent-local

build:
	go build ./...

test:
	go test ./...

# Lambda custom runtime wants a static arm64 binary named "bootstrap".
build-reaper:
	mkdir -p build/reaper
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o build/reaper/bootstrap ./cmd/reaper

ecr-login:
	aws ecr get-login-password --region $$($(TF) output -raw region 2>/dev/null || aws configure get region) \
		| docker login --username AWS --password-stdin $$($(TF) output -raw ecr_controlplane | cut -d/ -f1)

push-images: ecr-login
	docker build --platform linux/arm64 -f Dockerfile.controlplane -t $$($(TF) output -raw ecr_controlplane):latest .
	docker push $$($(TF) output -raw ecr_controlplane):latest
	docker build --platform linux/arm64 -f Dockerfile.agent -t $$($(TF) output -raw ecr_agent):latest .
	docker push $$($(TF) output -raw ecr_agent):latest

# One command, from zero. ECR repos first (images must exist before the ECS
# service and lambda can come up), then images, then everything else.
deploy: test build-reaper
	$(TF) init
	$(TF) apply -auto-approve $(TF_VARS) -target=aws_ecr_repository.controlplane -target=aws_ecr_repository.agent
	$(MAKE) push-images
	$(TF) apply -auto-approve $(TF_VARS)
	@echo "API URL (may take ~1 min for the service to start):"
	@scripts/api-url.sh || echo "  retry: make api-url"

destroy:
	$(TF) destroy -auto-approve $(TF_VARS)

api-url:
	@scripts/api-url.sh

# Mint a demo JWT: make token SUB=alice
token:
	@scripts/with-env.sh go run ./cmd/token -sub $(SUB)

# Run the controlplane on your laptop against the real AWS resources.
run-local:
	scripts/with-env.sh go run ./cmd/controlplane

# Iterate on the browser task with no AWS and no docker build.
agent-local:
	go run ./cmd/agent -local -prompt "golang generics" -out ./tmp

loadtest:
	scripts/loadtest.sh $$(scripts/api-url.sh)
