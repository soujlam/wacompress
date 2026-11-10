# wacompress build and install.
#
#   make                 build ./wacompress for this machine
#   make test            run vet and the tests
#   make install         install to $(PREFIX)/bin (default ~/.local/bin, no sudo needed)
#   make uninstall       remove it again
#   make dist            build binaries for Linux, macOS and Windows into dist/
#   make deps            install adb, ffmpeg and mkvtoolnix with the system package manager
#   make hdr10plus-tool  download hdr10plus_tool into $(PREFIX)/bin (needed to keep HDR10+)
#   make clean           remove build output

BINARY  := wacompress
PREFIX  ?= $(HOME)/.local
BINDIR  := $(PREFIX)/bin
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
GO      ?= go

# OS/architecture pairs built by "make dist".
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

# hdr10plus_tool (https://github.com/quietvoid/hdr10plus_tool) re-inserts HDR10+ metadata.
# The download is checked against a pinned SHA-256; other systems get the release link.
HDR10PLUS_VERSION := 1.7.2
HDR10PLUS_ASSET   := hdr10plus_tool-$(HDR10PLUS_VERSION)-x86_64-unknown-linux-musl.tar.gz
HDR10PLUS_SHA256  := 06385f37a639d61ba21d4be3150c863846933bc3b58110e094d8fc8f1c2249f2
HDR10PLUS_URL     := https://github.com/quietvoid/hdr10plus_tool/releases/download/$(HDR10PLUS_VERSION)
HDR10PLUS_TMP     := .hdr10plus

.PHONY: all build test install uninstall dist deps hdr10plus-tool clean help

all: build

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	$(GO) vet ./...
	$(GO) test ./...

install: build
	mkdir -p "$(BINDIR)"
	install -m 0755 $(BINARY) "$(BINDIR)/$(BINARY)"
	@echo "Installed to $(BINDIR)/$(BINARY)"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "Note: $(BINDIR) is not in your PATH.";; esac

uninstall:
	rm -f "$(BINDIR)/$(BINARY)"

dist:
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; \
		[ $$os = windows ] && ext=.exe; \
		out=$(DIST)/$(BINARY)-$(VERSION)-$$os-$$arch$$ext; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -ldflags "$(LDFLAGS)" -o $$out . || exit 1; \
	done

# Installs the external tools the program calls. Needs sudo on Linux.
deps:
	@if command -v apt-get >/dev/null 2>&1; then \
		sudo apt-get install -y adb ffmpeg mkvtoolnix; \
	elif command -v dnf >/dev/null 2>&1; then \
		echo "Fedora: the default ffmpeg-free has no libx265; enable RPM Fusion and install ffmpeg from there."; \
		sudo dnf install -y android-tools ffmpeg mkvtoolnix; \
	elif command -v pacman >/dev/null 2>&1; then \
		sudo pacman -S --needed android-tools ffmpeg mkvtoolnix-cli; \
	elif command -v zypper >/dev/null 2>&1; then \
		sudo zypper install -y android-tools ffmpeg mkvtoolnix; \
	elif command -v brew >/dev/null 2>&1; then \
		brew install ffmpeg mkvtoolnix && brew install --cask android-platform-tools; \
	else \
		echo "No supported package manager found. Install adb, ffmpeg and mkvtoolnix by hand."; \
		echo "Windows: winget install Gyan.FFmpeg Google.PlatformTools MoritzBunkus.MKVToolNix"; \
		exit 1; \
	fi

hdr10plus-tool:
	@if [ "$$(uname -s)-$$(uname -m)" != "Linux-x86_64" ]; then \
		echo "Download hdr10plus_tool for your system from $(HDR10PLUS_URL)"; \
		echo "and put it in a folder in your PATH."; exit 1; \
	fi
	mkdir -p "$(BINDIR)" $(HDR10PLUS_TMP)
	curl -fsSL -o $(HDR10PLUS_TMP)/$(HDR10PLUS_ASSET) $(HDR10PLUS_URL)/$(HDR10PLUS_ASSET)
	echo "$(HDR10PLUS_SHA256)  $(HDR10PLUS_TMP)/$(HDR10PLUS_ASSET)" | sha256sum -c -
	tar -xzf $(HDR10PLUS_TMP)/$(HDR10PLUS_ASSET) -C $(HDR10PLUS_TMP) ./hdr10plus_tool
	install -m 0755 $(HDR10PLUS_TMP)/hdr10plus_tool "$(BINDIR)/hdr10plus_tool"
	rm -f $(HDR10PLUS_TMP)/hdr10plus_tool $(HDR10PLUS_TMP)/$(HDR10PLUS_ASSET)
	rmdir $(HDR10PLUS_TMP)
	@echo "Installed to $(BINDIR)/hdr10plus_tool"

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

help:
	@sed -n '1,/^$$/p' Makefile | sed 's/^# \{0,1\}//'
