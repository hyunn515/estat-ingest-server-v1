APP_NAME       := estat-server

# === ECR 설정 (예시) ===
ECR_ACCOUNT_ID := 1111111111
ECR_REGION     := ap-northeast-2
ECR_REPOSITORY := $(APP_NAME)

# git short sha 또는 브랜치명 등을 TAG로 사용할 수 있음
IMAGE_TAG ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "local")
IMAGE_URI := $(ECR_ACCOUNT_ID).dkr.ecr.$(ECR_REGION).amazonaws.com/$(ECR_REPOSITORY):$(IMAGE_TAG)

# 빌드 타겟 플랫폼 (ECS Fargate 대부분은 linux/amd64, Graviton이면 arm64)
PLATFORM ?= linux/amd64

.PHONY: build
build:
	docker build \
	  --platform=$(PLATFORM) \
	  -t $(IMAGE_URI) \
	  .

.PHONY: push
push:
	aws ecr get-login-password --region $(ECR_REGION) \
	  | docker login \
	      --username AWS \
	      --password-stdin $(ECR_ACCOUNT_ID).dkr.ecr.$(ECR_REGION).amazonaws.com
	docker push $(IMAGE_URI)

.PHONY: run-local
run-local:
	docker run --rm -it \
	  -p 8080:8080 \
	  --env-file .env \
	  $(IMAGE_URI)

.PHONY: print-image
print-image:
	@echo $(IMAGE_URI)