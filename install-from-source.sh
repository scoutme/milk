#!/bin/sh
# milk Installation Script (build from source)
# Usage: curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh
#
# Environment variables:
#   MILK_INSTALL_DIR - Installation directory (default: ~/.local/share/milk)
#   MILK_VERSION     - Git tag/branch to install (default: main)

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

detect_os() {
    case "$(uname -s)" in
        Linux*)  OS_TYPE="linux";;
        Darwin*) OS_TYPE="macos";;
        CYGWIN*|MINGW*|MSYS*)
            error "Windows is not natively supported. Please use WSL2 instead.";;
        *)
            error "Unsupported operating system: $(uname -s)";;
    esac
}

check_go() {
    info "Checking Go installation..."
    if ! check_cmd go; then
        error "Go is required but not installed.

    Installation instructions:
    - macOS: brew install go
    - Ubuntu/Debian: sudo apt install golang-go   (or https://go.dev/dl/)
    - Fedora: sudo dnf install golang"
    fi
    GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    GO_MAJOR=$(echo "$GO_VERSION" | cut -d. -f1)
    GO_MINOR=$(echo "$GO_VERSION" | cut -d. -f2)
    if [ "$GO_MAJOR" -lt 1 ] || { [ "$GO_MAJOR" -eq 1 ] && [ "$GO_MINOR" -lt 21 ]; }; then
        error "Go 1.21 or higher is required (found $GO_VERSION). Please upgrade: https://go.dev/dl/"
    fi
    success "Found Go $GO_VERSION"
}

check_git() {
    info "Checking Git installation..."
    if check_cmd git; then
        success "Found Git"
    else
        error "Git is required but not installed.

    Installation instructions:
    - macOS: xcode-select --install
    - Ubuntu/Debian: sudo apt install git
    - Fedora: sudo dnf install git"
    fi
}

clone_repository() {
    VERSION="${MILK_VERSION:-main}"
    REPO_URL="https://github.com/scoutme/milk.git"

    if [ -d "$INSTALL_DIR/.git" ]; then
        info "Updating existing installation..."
        cd "$INSTALL_DIR"
        git fetch origin
        git checkout "$VERSION"
        git pull origin "$VERSION" 2>/dev/null || true
    else
        info "Cloning milk repository..."
        mkdir -p "$(dirname "$INSTALL_DIR")"
        git clone --branch "$VERSION" "$REPO_URL" "$INSTALL_DIR"
        cd "$INSTALL_DIR"
    fi

    success "Repository ready at $INSTALL_DIR"
}

build_and_install() {
    info "Building milk..."
    cd "$INSTALL_DIR"
    go build -o "$BIN_DIR/milk" ./cmd/milk/
    chmod +x "$BIN_DIR/milk"
    success "Installed milk to $BIN_DIR/milk"
}

print_success() {
    SHELL_NAME="$(basename "$SHELL")"

    echo ""
    printf "${GREEN}${BOLD}"
    echo "============================================"
    echo "  milk installed successfully!"
    echo "============================================"
    printf "${NC}"
    echo ""
    echo "Binary: $BIN_DIR/milk"
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
    echo "2. Ensure llama.cpp is running (default: http://localhost:8080)"
    echo "   and the claude CLI is available in PATH."
    echo ""
    echo "3. Start milk:"
    printf "   ${BLUE}milk${NC}              # interactive mode\n"
    printf "   ${BLUE}milk 'your prompt'${NC} # single-shot mode\n"
    printf "   ${BLUE}milk --help${NC}       # all options\n"
    echo ""
    echo "Config file: ~/.milk/config.json (created on first run)"
    echo ""
    echo "For more information:"
    echo "https://github.com/scoutme/milk"
    echo ""
}

main() {
    detect_os

    INSTALL_DIR="${MILK_INSTALL_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/milk}"
    BIN_DIR="$HOME/.local/bin"
    mkdir -p "$BIN_DIR"

    info "Installing milk from source..."
    echo "  Source directory: $INSTALL_DIR"
    echo "  Binary:           $BIN_DIR/milk"
    echo ""

    check_git
    check_go
    clone_repository
    build_and_install
    print_success
}

main "$@"
