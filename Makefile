GO                    := GO111MODULE=on go
GIT_HEAD_COMMIT_LONG  := $(shell git log -1 --format='%H')
GIT_HEAD_COMMIT_SHORT := $(shell git rev-parse --short HEAD)
GIT_HEAD_ABBREV       := $(shell git rev-parse --abbrev-ref HEAD)

include .makerc

BUILD_TAGS             ="osusergo,netgo,ledger,mainnet,static_build"

ldflags = -X github.com/cosmos/cosmos-sdk/version.Name=akash \
-X github.com/cosmos/cosmos-sdk/version.AppName=akash \
-X "github.com/cosmos/cosmos-sdk/version.BuildTags=$(BUILD_TAGS)" \
-X github.com/cosmos/cosmos-sdk/version.Version=$(shell git describe --tags | sed 's/^v//') \
-X github.com/cosmos/cosmos-sdk/version.Commit=$(GIT_HEAD_COMMIT_LONG)

# check for nostrip option
ifeq (,$(findstring nostrip,$(BUILD_OPTIONS)))
	ldflags += -s -w
endif
ldflags += $(LDFLAGS)
ldflags := $(strip $(ldflags))

BUILD_FLAGS := -mod=readonly -tags "$(BUILD_TAGS)" -ldflags '$(ldflags)'
# check for nostrip option
ifeq (,$(findstring nostrip,$(BUILD_OPTIONS)))
	BUILD_FLAGS += -trimpath
endif

GORELEASER_SKIP_VALIDATE = false
GORELEASER_CONFIG        = .goreleaser.yaml
GORELEASER_BUILD_TAGS    = $(BUILD_TAGS)
GORELEASER_FLAGS         = -tags="$(GORELEASER_BUILD_TAGS)"
GORELEASER_LD_FLAGS      = -s -w -X github.com/cosmos/cosmos-sdk/version.Name=akash \
-X github.com/cosmos/cosmos-sdk/version.AppName=akash \
-X github.com/cosmos/cosmos-sdk/version.BuildTags="$(GORELEASER_BUILD_TAGS)" \
-X github.com/cosmos/cosmos-sdk/version.Version=$(GORELEASER_TAG) \
-X github.com/cosmos/cosmos-sdk/version.Commit=$(GIT_HEAD_COMMIT_LONG)

.PHONY: e2e
e2e:
	$(GO) build $(BUILD_FLAGS) ./cmd/e2e

.PHONY: release-dry-run
release-dry-run:
	BUILD_FLAGS="$(GORELEASER_FLAGS)" LD_FLAGS="$(GORELEASER_LD_FLAGS)" goreleaser \
		-f "$(GORELEASER_CONFIG)" \
		--skip-validate=$(GORELEASER_SKIP_VALIDATE) \
		--rm-dist \
		--skip-publish

.PHONY: release
release:
	BUILD_FLAGS="$(GORELEASER_FLAGS)" LD_FLAGS="$(GORELEASER_LD_FLAGS)" goreleaser \
		-f "$(GORELEASER_CONFIG)" \
		release \
		--rm-dist

.PHONY: clean
clean:
	rm -f e2e
