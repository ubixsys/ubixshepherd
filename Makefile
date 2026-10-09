# Build and check uBixShepherd. CI runs the same targets.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ubixsys/ubixshepherd/internal/version.Version=$(VERSION)
TARGETS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# Core code must not name a product: product knowledge lives in packs (design.md §3.12).
CORE_DIRS := cmd internal
PRODUCT_PATTERN := ubixcore|ubixvault|ubixops|replikate|ubixos

PREFIX ?= $(HOME)/.local/bin

.PHONY: build install test check core-boundary cross dist release-notes clean

build:
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/shepherd ./cmd/shepherd

# Copy the binary to a stable path (the one `shepherd daemon install` should register),
# replacing it atomically, and restart the daemon if one is running so it runs the new
# build.
install: build
	@mkdir -p $(PREFIX)
	@cp bin/shepherd $(PREFIX)/.shepherd.new && mv -f $(PREFIX)/.shepherd.new $(PREFIX)/shepherd
	@echo "installed $(PREFIX)/shepherd ($(VERSION))"
	@if SHEPHERD_NO_AUTOSTART=1 $(PREFIX)/shepherd daemon status | grep -q 'running, pid'; then \
		$(PREFIX)/shepherd daemon restart && \
		echo "the daemon runs in the background now; watch it: tail -f $${SHEPHERD_HOME:-$$HOME/.shepherd}/daemon.log"; \
	fi

test:
	go test ./...

check: core-boundary
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	go vet ./...
	go test ./...

core-boundary:
	@if grep -rniE '$(PRODUCT_PATTERN)' $(CORE_DIRS); then \
		echo "Core code names a product; move it into a pack."; exit 1; \
	fi
	@echo "Core is product-free."

cross:
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "build $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -ldflags '$(LDFLAGS)' \
			-o dist/shepherd-$$os-$$arch$$ext ./cmd/shepherd || exit 1; \
	done

# Release archives from the cross builds: one per target, each holding the binary, LICENSE
# and README.md (.tar.gz, or .zip for Windows), and SHA256SUMS over them. The release
# workflow runs this; run it locally to see exactly what a release would publish.
dist: cross
	@rm -rf dist/pkg dist/*.tar.gz dist/*.zip dist/SHA256SUMS
	@for t in $(TARGETS); do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		name=shepherd-$(VERSION)-$$os-$$arch; \
		mkdir -p dist/pkg/$$name && \
		cp dist/shepherd-$$os-$$arch$$ext dist/pkg/$$name/shepherd$$ext && \
		cp LICENSE README.md dist/pkg/$$name/ || exit 1; \
		if [ $$os = windows ]; then \
			(cd dist/pkg && zip -qr ../$$name.zip $$name) || exit 1; \
		else \
			COPYFILE_DISABLE=1 tar -C dist/pkg -czf dist/$$name.tar.gz $$name || exit 1; \
		fi; \
		echo "package dist/$$name"; \
	done
	@rm -rf dist/pkg
	@cd dist && if command -v sha256sum >/dev/null; then sum='sha256sum'; else sum='shasum -a 256'; fi && \
		$$sum *.tar.gz *.zip > SHA256SUMS
	@echo "wrote dist/SHA256SUMS"

# Print the CHANGELOG section for VERSION (v0.1.0 reads "## [0.1.0]"), or fail if there
# is none. The release workflow uses it for the release notes.
release-notes:
	@awk -v v="$(patsubst v%,%,$(VERSION))" ' \
		index($$0, "## [" v "]") == 1 { found = 1; next } \
		found && /^## \[/ { exit } \
		found { print } \
		END { if (!found) { print "CHANGELOG.md has no section for " v > "/dev/stderr"; exit 1 } }' CHANGELOG.md

clean:
	rm -rf bin dist
