CURRENT_DIR=$(shell pwd)
# APP = repo/dir name -> image ${REGISTRY}/${PROJECT_NAME}/${APP}
APP=$(shell basename ${CURRENT_DIR})

# Defaults; the CI (Ucode-io/ci-cd build.yml) overrides REGISTRY/PROJECT_NAME/TAG/ENV_TAG.
REGISTRY=ghcr.io
PROJECT_NAME=ucode-io
TAG=latest
ENV_TAG=latest
DOCKERFILE=Dockerfile

build-image:
	docker build --rm -t ${REGISTRY}/${PROJECT_NAME}/${APP}:${TAG} . -f ${DOCKERFILE}
	docker tag ${REGISTRY}/${PROJECT_NAME}/${APP}:${TAG} ${REGISTRY}/${PROJECT_NAME}/${APP}:${ENV_TAG}

push-image:
	docker push ${REGISTRY}/${PROJECT_NAME}/${APP}:${TAG}
	docker push ${REGISTRY}/${PROJECT_NAME}/${APP}:${ENV_TAG}

clear-image:
	docker rmi ${REGISTRY}/${PROJECT_NAME}/${APP}:${TAG}
	docker rmi ${REGISTRY}/${PROJECT_NAME}/${APP}:${ENV_TAG}

.PHONY: build-image push-image clear-image
