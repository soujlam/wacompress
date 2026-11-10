# wacompress build and install.
#
#   make              build ./wacompress for this machine
#   make test         run vet and the tests
#   make install      install to $(PREFIX)/bin (default ~/.local/bin, no sudo needed)
#   make uninstall    remove it again
#   make dist         build binaries for Linux, macOS and Windows into dist/
#   make deps         install adb and ffmpeg with the system package manager
#   make clean        remove build output

BINARY  := wacompress
PREFIX  ?= $(HOME)/.local
BINDIR  := $(PREFIX)/bin
DIST    := dist
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w
GO      ?= go

# OS/architecture pairs built by "make dist".
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build test install uninstall dist deps clean help

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
		sudo apt-get install -y adb ffmpeg; \
	elif command -v dnf >/dev/null 2>&1; then \
		echo "Fedora: the default ffmpeg-free has no libx265; enable RPM Fusion and install ffmpeg from there."; \
		sudo dnf install -y android-tools ffmpeg; \
	elif command -v pacman >/dev/null 2>&1; then \
		sudo pacman -S --needed android-tools ffmpeg; \
	elif command -v zypper >/dev/null 2>&1; then \
		sudo zypper install -y android-tools ffmpeg; \
	elif command -v brew >/dev/null 2>&1; then \
		brew install ffmpeg && brew install --cask android-platform-tools; \
	else \
		echo "No supported package manager found. Install adb and ffmpeg by hand."; \
		echo "Windows: winget install Gyan.FFmpeg Google.PlatformTools"; \
		exit 1; \
	fi

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

help:
	@sed -n '1,/^$$/p' Makefile | sed 's/^# \{0,1\}//'
