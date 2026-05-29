#!/bin/sh
# milk Installation Script
# Usage: curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
#
# Environment variables:
#   MILK_VERSION - Release tag to install, e.g. v0.2.0 (default: latest)

set -e

GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
RED='\033[0;31m'
BOLD='\033[1m'
NC='\033[0m'

info()    { printf "${BLUE}==>${NC} ${BOLD}%s${NC}\n" "$1"; }
success() { printf "${GREEN}==>${NC} ${BOLD}%s${NC}\n" "$1"; }
warn()    { printf "${YELLOW}Warning:${NC} %s\n" "$1"; }
error()   { printf "${RED}Error:${NC} %s\n" "$1" >&2; exit 1; }
check_cmd() { command -v "$1" >/dev/null 2>&1; }

REPO="scoutme/milk"
BIN_DIR="$HOME/.local/bin"

detect_platform() {
    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux*)  GOOS="linux";;
        Darwin*) GOOS="darwin";;
        CYGWIN*|MINGW*|MSYS*)
            warn "Native Windows is not fully supported. Using WSL2 is recommended."
            GOOS="windows";;
        *) error "Unsupported operating system: $OS";;
    esac

    case "$ARCH" in
        x86_64|amd64) GOARCH="amd64";;
        aarch64|arm64) GOARCH="arm64";;
        *) error "Unsupported architecture: $ARCH";;
    esac

    EXT=""
    [ "$GOOS" = "windows" ] && EXT=".exe"
}

resolve_version() {
    if [ -n "$MILK_VERSION" ]; then
        VERSION="$MILK_VERSION"
        return
    fi
    info "Resolving latest release..."
    if check_cmd curl; then
        VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
            | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
    elif check_cmd wget; then
        VERSION=$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" \
            | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
    else
        error "curl or wget is required to download milk."
    fi
    [ -n "$VERSION" ] || error "No published releases found. Set MILK_VERSION to a specific tag, or build from source:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install-from-source.sh | sh"
}

download_binary() {
    BINARY_NAME="milk-${GOOS}-${GOARCH}${EXT}"
    BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
    BINARY_URL="${BASE_URL}/${BINARY_NAME}"
    CHECKSUM_URL="${BASE_URL}/${BINARY_NAME}.sha256"
    TMP_DIR="$(mktemp -d)"
    TMP_BIN="${TMP_DIR}/milk${EXT}"
    TMP_SHA="${TMP_DIR}/milk.sha256"

    info "Downloading milk ${VERSION} (${GOOS}/${GOARCH})..."

    if check_cmd curl; then
        curl -fsSL "$BINARY_URL" -o "$TMP_BIN" || return 1
        curl -fsSL "$CHECKSUM_URL" -o "$TMP_SHA" 2>/dev/null || true
    elif check_cmd wget; then
        wget -qO "$TMP_BIN" "$BINARY_URL" || return 1
        wget -qO "$TMP_SHA" "$CHECKSUM_URL" 2>/dev/null || true
    fi

    # Verify checksum when sha256sum/shasum is available and we got a checksum file.
    if [ -s "$TMP_SHA" ]; then
        EXPECTED=$(awk '{print $1}' "$TMP_SHA")
        if check_cmd sha256sum; then
            ACTUAL=$(sha256sum "$TMP_BIN" | awk '{print $1}')
        elif check_cmd shasum; then
            ACTUAL=$(shasum -a 256 "$TMP_BIN" | awk '{print $1}')
        else
            ACTUAL=""
        fi
        if [ -n "$ACTUAL" ] && [ "$ACTUAL" != "$EXPECTED" ]; then
            rm -rf "$TMP_DIR"
            error "Checksum mismatch for $BINARY_NAME (expected $EXPECTED, got $ACTUAL)."
        fi
        [ -n "$ACTUAL" ] && success "Checksum verified"
    fi

    mkdir -p "$BIN_DIR"
    mv "$TMP_BIN" "$BIN_DIR/milk${EXT}"
    chmod +x "$BIN_DIR/milk${EXT}"
    rm -rf "$TMP_DIR"
    success "Installed milk ${VERSION} to ${BIN_DIR}/milk${EXT}"
}

# ── success message ───────────────────────────────────────────────────────────

print_success() {
    SHELL_NAME="$(basename "$SHELL")"
    echo ""
    printf "${GREEN}${BOLD}"
    echo "============================================"
    echo "  milk installed successfully!"
    echo "============================================"
    printf "${NC}"
    echo ""
    echo "Binary: $BIN_DIR/milk${EXT}"
    echo ""
    printf "${BOLD}Next steps:${NC}\n"
    echo ""
    echo "1. Add ~/.local/bin to your PATH (if not already):"
    echo ""
    case "$SHELL_NAME" in
        zsh)  printf "   ${BLUE}echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.zshrc && source ~/.zshrc${NC}\n";;
        bash) printf "   ${BLUE}echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.bashrc && source ~/.bashrc${NC}\n";;
        fish) printf "   ${BLUE}fish_add_path ~/.local/bin${NC}\n";;
        *)    printf "   ${BLUE}export PATH=\"\$HOME/.local/bin:\$PATH\"${NC}\n";;
    esac
    echo ""
    echo "2. Ensure your local inference server is running (or configure a cloud provider)."
    echo "   and the claude CLI is available in PATH."
    echo ""
    echo "3. Start milk:"
    printf "   ${BLUE}milk${NC}              # interactive mode\n"
    printf "   ${BLUE}milk 'your prompt'${NC} # single-shot mode\n"
    printf "   ${BLUE}milk --help${NC}       # all options\n"
    echo ""
    echo "Config file: ~/.milk/config.json (created on first run)"
    echo ""
    echo "For more information: https://github.com/${REPO}"
    echo ""
}

# ── main ──────────────────────────────────────────────────────────────────────

main() {
    detect_platform
    resolve_version

    if ! download_binary; then
        error "Pre-built binary not available for ${GOOS}/${GOARCH}.
To build from source instead, run:
  curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install-from-source.sh | sh"
    fi

    print_success
}

main "$@"
