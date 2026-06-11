# WaxSeal build orchestration.
#
# WaxSeal mints YouTube PO tokens from a real headless Chromium (go-rod). The
# only build artifact is the bgutils + BotGuard bundle embedded in
# internal/browser (built with Node/esbuild from build/js). It is committed, so
# `go build`/`go test` need no Node toolchain. The CLI/daemon requires a system
# Chromium at runtime (not bundled).

VERSION           ?= dev
DIST              := dist
RELEASE_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

BROWSER_BUNDLE_OUT := internal/browser/bg_browser_bundle.js

REGISTRY    ?= ghcr.io
IMAGE_OWNER ?= colespringer
IMAGE       := $(REGISTRY)/$(IMAGE_OWNER)/waxseal

# PUSH_LATEST gates whether docker-push moves the :latest tag. Default 0 (publish
# only the VERSION tag); set PUSH_LATEST=1 for a real release that should move latest,
# so an out-of-band hotfix can't repoint :latest by accident.
PUSH_LATEST ?= 0

.PHONY: all test jsbundle-browser verify-assets release deps clean \
        docker-build docker-login docker-push release-guard

all: jsbundle-browser

# test runs the offline suite. The committed bundle means no Node toolchain is
# needed; this is what CI and `go test ./...` both exercise.
test:
	go test ./...

# jsbundle-browser builds the bgutils-js + BotGuard entrypoint as an
# ES2020 IIFE that is eval'd into the real Chromium. Committed + go:embed-ed in
# internal/browser.
jsbundle-browser: $(BROWSER_BUNDLE_OUT)

$(BROWSER_BUNDLE_OUT): build/js/build-browser.mjs build/js/browser_entrypoint.js build/js/package.json build/js/package-lock.json
	cd build/js && npm ci --no-audit --no-fund --silent && node build-browser.mjs
	@echo "built $@ ($$(wc -c < $@) bytes)"

# verify-assets rebuilds the embedded bundle and fails if it differs from the
# committed bytes (reproducibility check for CI).
verify-assets:
	rm -f $(BROWSER_BUNDLE_OUT)
	$(MAKE) jsbundle-browser
	@git diff --exit-code -- $(BROWSER_BUNDLE_OUT) \
	  && echo "OK: rebuilt bundle reproduces the committed bytes" \
	  || { echo "ERROR: rebuilt bundle differs from the committed bytes"; exit 1; }

# release cross-compiles the CLI/daemon for the GOOS/GOARCH matrix into dist/.
# Each binary embeds the JS bundle but requires a system Chromium at runtime.
release:
	@mkdir -p $(DIST)
	@for p in $(RELEASE_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; ext=""; [ "$$os" = "windows" ] && ext=".exe"; \
	  out=$(DIST)/waxseal-$$os-$$arch$$ext; \
	  echo "building $$out"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath \
	    -ldflags "-s -w -X main.version=$(VERSION)" -o $$out ./cmd/waxseal || exit 1; \
	done
	@echo "release binaries in $(DIST)/ (each requires a system Chromium at runtime)"

# Publish the runtime image to GitHub Container Registry. Auth reuses your gh
# login: the token is piped to docker on stdin, so it never lands in args, env,
# or shell history. Publish 1.0.0 and move :latest with:
#   PUSH_LATEST=1 make docker-push VERSION=1.0.0

# docker-build builds the runtime image, tagged VERSION and latest.
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

# release-guard refuses to publish the default/empty VERSION, which would tag an
# unreleased build and (with PUSH_LATEST=1) repoint the public :latest at it.
release-guard:
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "dev" ]; then \
	  echo "ERROR: docker-push needs VERSION=x.y.z (not empty or 'dev')"; exit 1; fi

# docker-login signs in to GHCR with the gh token (never printed). It distinguishes
# "not logged in" from "logged in but missing the write:packages scope" and prints
# the one-time fix for each.
docker-login:
	@gh auth status >/dev/null 2>&1 || { \
	  echo "not logged in to gh. Run once, then retry:"; \
	  echo "    gh auth login"; \
	  exit 1; }
	@gh api -i user 2>/dev/null | grep -qi '^X-Oauth-Scopes:.*write:packages' || { \
	  echo "gh token is missing the 'write:packages' scope. Run once, then retry:"; \
	  echo "    gh auth refresh -h github.com -s write:packages"; \
	  exit 1; }
	@gh auth token | docker login $(REGISTRY) -u $(IMAGE_OWNER) --password-stdin

# docker-push validates VERSION + auth (release-guard, docker-login) BEFORE the build,
# then pushes the VERSION tag, plus :latest only when PUSH_LATEST=1.
docker-push: release-guard docker-login docker-build
	docker push $(IMAGE):$(VERSION)
	@if [ "$(PUSH_LATEST)" = "1" ]; then \
	  docker push $(IMAGE):latest && echo "pushed $(IMAGE):$(VERSION) and moved :latest"; \
	else \
	  echo "pushed $(IMAGE):$(VERSION) (PUSH_LATEST=0; :latest not moved)"; \
	fi

# deps installs the Node toolchain used to rebuild the browser bundle
# (deterministically, from the committed lockfile).
deps:
	cd build/js && npm ci --no-audit --no-fund

clean:
	rm -rf $(DIST)
