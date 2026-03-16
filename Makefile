.PHONY: all
all: bin/kube-headscale

GOFILES=$(shell find ./ -name '*.go' -not -name '*_test.go')
GOTESTFILES=$(shell find ./ -name '*_test.go')

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
