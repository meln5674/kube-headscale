.PHONY: all
all: bin/kube-headscale

GOFILES=$(shell find ./ -name '*.go' -not -name '*_test.go')
GOTESTFILES=$(shell find ./ -name '*_test.go')

.PHONY: lint
lint:
	go vet ./...
	helm lint ./deploy/helm/kube-headscale/

bin/kube-headscale: $(GOFILES)
	go build -o $@


bin/kube-headscale.cover: $(GOFILES)
	go build -o $@ -cover


GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)

HEADSCALE_MIRROR ?= https://github.com/juanfont/headscale/releases/download
HEADSCALE_VERSION ?= 0.28.0
HEADSCALE_URL ?= $(HEADSCALE_MIRROR)/v$(HEADSCALE_VERSION)/headscale_$(HEADSCALE_VERSION)_$(GOOS)_$(GOARCH)

HEADSCALE ?= bin/headscale

bin/headscale:
	wget -O $@ $(HEADSCALE_URL)
	chmod +x $@


TAILSCALE_MIRROR ?= https://pkgs.tailscale.com/stable
TAILSCALE_VERSION ?= 1.94.2
TAILSCALE_URL ?= $(TAILSCALE_MIRROR)/tailscale_$(TAILSCALE_VERSION)_$(GOARCH).tgz

TAILSCALE_TGZ=bin/tailscale_$(TAILSCALE_VERSION)_$(GOARCH).tgz
TAILSCALE_TGZ_DIR=tailscale_$(TAILSCALE_VERSION)_$(GOARCH)

$(TAILSCALE_TGZ):
	wget -O $@ $(TAILSCALE_URL)

bin/tailscale bin/tailscaled &: $(TAILSCALE_TGZ)
	tar -xzf $^ -C bin --strip-components=1 $(TAILSCALE_TGZ_DIR)/tailscale $(TAILSCALE_TGZ_DIR)/tailscaled
	touch -c bin/tailscale bin/tailscaled


ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
LOCALBIN ?= bin
ENVTEST ?= go tool setup-envtest
ENVTEST_BINDIR="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -i --bin-dir $(LOCALBIN) -p path)"

ENVTEST_SUBDIR=$(ENVTEST_K8S_VERSION).0-$(GOOS)-$(GOARCH)
ENVTEST_BINS = \
	$(LOCALBIN)/k8s/$(ENVTEST_SUBDIR)/kubectl \
	$(LOCALBIN)/k8s/$(ENVTEST_SUBDIR)/kube-apiserver \
	$(LOCALBIN)/k8s/$(ENVTEST_SUBDIR)/etcd
$(ENVTEST_BINS):
	$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path

GINKGO ?= go tool ginkgo
GOCOVERDIR ?= $(LOCALBIN)/gocoverdir

bin/coverprofile.out: $(HEADSCALE) bin/kube-headscale.cover $(ENVTEST_BINS) $(GOTESTFILES)
	rm -rf $(GOCOVERDIR)
	mkdir $(GOCOVERDIR)
	GOCOVERDIR=$(GOCOVERDIR) KUBEBUILDER_ASSETS=$(ENVTEST_BINDIR) $(GINKGO) run $(GINKGO_ARGS)
	go tool covdata textfmt -i=$(GOCOVERDIR) -o $@

.PHONY: test
test: bin/coverprofile.out

.PHONY: coverage
coverage: bin/coverprofile.out
	go tool cover -html=$^


DOCKER=docker
HELM=helm

CHART_DIR=./deploy/helm/kube-headscale/

IMAGE_REGISTRY ?= ghcr.io
IMAGE_REPO ?= meln5674/kube-headscale
IMAGE_TAG ?= latest
IMAGE_REF ?= $(IMAGE_REGISTRY)/$(IMAGE_REPO):$(IMAGE_TAG)

.PHONY: image push-image
image:
	$(DOCKER) build -t $(IMAGE_REF) .

DEBUG_IMAGE_TAG ?= $(IMAGE_TAG)-debug
DEBUG_IMAGE_REF ?= $(IMAGE_REGISTRY)/$(IMAGE_REPO):$(IMAGE_TAG)
push-image:
	$(DOCKER) push $(IMAGE_REF)


.PHONY: debug-image
debug-image: bin/kube-headscale
	$(DOCKER) build -t $(DEBUG_IMAGE_REF) -f debug.Dockerfile .

CHART_YAML=$(CHART_DIR)/Chart.yaml
CHART_NAME=$(shell yq .name $(CHART_YAML))
CHART_VERSION=$(shell yq .version $(CHART_YAML))
CHART_APP_VERSION ?=

CHART_REMOTE ?= oci://$(IMAGE_REGISTRY)/$(CHART_REPO)
CHART_REPO ?= $(IMAGE_REPO)/chart/

CHART_TARBALL_DIR ?= bin
CHART_TARBALL ?= $(CHART_TARBALL_DIR)/$(CHART_NAME)-$(CHART_VERSION).tgz

$(CHART_TARBALL):
	$(HELM) package $(CHART_DIR) --destination $(CHART_TARBALL_DIR) --version '$(IMAGE_TAG)'

.PHONY: chart
chart: $(CHART_TARBALL)

CHART_TAG_FILE ?= $(CHART_TARBALL).tag
CHART_DIGEST_FILE ?= $(CHART_TARBALL).digest

.PHONY: push-chart
push-chart:
	out="$$($(HELM) push $(CHART_TARBALL) $(CHART_REMOTE))" \
	&& echo "$${out}" | grep Digest: | awk '{ print $$2 }'

