IMAGE_NAME = ghcr.io/infosecstreams/streamstatus
IMAGE_TAG = $(shell git describe --tags --always --dirty --long)

.PHONY: help
help:
	@echo "make build-ss"
	@echo "make run-ss"
	@echo "make push-ss"
	@echo "make all"


.PHONY: build-ss
build-ss:
	docker build --build-arg VERSION=$(IMAGE_TAG) -t $(IMAGE_NAME):$(IMAGE_TAG) .

.PHONY: run-ss
run-ss: build-ss
	docker run -it --rm -p 8080:8080 \
	-e SS_SECRETKEY=secret \
	-e SS_TOKEN=token \
	-e SS_USERNAME=username \
	-e TW_CLIENT_ID=client_id \
	-e TW_CLIENT_SECRET=client_secret \
	-e SS_PUSHBULLET_APIKEY=myAPIkey \
	-e SS_PUSHBULLET_DEVICES=myDevice,anotherDevice \
	$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: push
push-ss: build-ss
	@echo "Are you sure you want to push? [y/N] " && read ans && [ $${ans:-N} = y ]
	docker tag $(IMAGE_NAME):$(IMAGE_TAG) $(IMAGE_NAME):latest
	docker push $(IMAGE_NAME):$(IMAGE_TAG)
	docker push $(IMAGE_NAME):latest

.PHONY: all
all: push-ss

clean:
	docker rmi $(IMAGE_NAME):$(IMAGE_TAG)
