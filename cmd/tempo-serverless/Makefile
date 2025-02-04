# todo: get docker run working?
# PACK=docker run -u ${shell id -u} -v /var/run/docker.sock:/var/run/docker.sock -v $(PWD):/workspace -w /workspace buildpacksio/pack
PACK=pack
VERSION=$(shell ../../tools/image-tag | cut -d, -f 1)

IN_CLOUD_FUNCTIONS=cd cloud-functions &&
IN_LAMBDA=cd lambda &&

#
# build docker images for local testing and code zip files for google cloud functions
#
.PHONY: build-docker
build-docker: build-docker-gcf build-docker-lambda-test

.PHONY: build-docker-gcf
build-docker-gcf: 
	$(IN_CLOUD_FUNCTIONS) go mod vendor
	$(IN_CLOUD_FUNCTIONS) $(PACK) build tempo-serverless \
	  								--builder gcr.io/buildpacks/builder:v1 \
	  								--env GOOGLE_RUNTIME=go \
	  								--env GOOGLE_FUNCTION_SIGNATURE_TYPE=http \
	  								--env GOOGLE_FUNCTION_TARGET=Handler 	  
	$(IN_CLOUD_FUNCTIONS) rm -rf vendor

.PHONY: build-gcf-zip
build-gcf-zip:
	$(IN_CLOUD_FUNCTIONS) go mod vendor
	$(IN_CLOUD_FUNCTIONS) zip tempo-serverless-$(VERSION).zip ./* -r
	$(IN_CLOUD_FUNCTIONS) rm -rf vendor

#
# build docker images for local testing and code zip files for aws lambda
#
.PHONY: build-docker-lambda-test
build-docker-lambda-test:
	$(IN_LAMBDA) CGO_ENABLED=0 go build -o ./lambda
	$(IN_LAMBDA) docker build -f ./Dockerfile -t tempo-serverless-lambda .

# lambda zips expect a compiled executable in root. the filename "main" is important
# as that should the handler config option in aws
.PHONY: build-lambda-zip
build-lambda-zip:
	$(IN_LAMBDA) GOOS=linux GOARCH=amd64 go build -o main
	$(IN_LAMBDA) zip tempo-serverless-$(VERSION).zip main
	$(IN_LAMBDA) rm main

.PHONY: test
test:
	go test -v .

### Tidy dependencies for tempo-serverless module
.PHONY: update-mod
update-mod:
	$(IN_LAMBDA) go mod tidy -e
	$(IN_CLOUD_FUNCTIONS) go mod tidy -e
