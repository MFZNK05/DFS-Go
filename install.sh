#!/bin/sh
# Hermod installer — detects OS/arch, downloads latest release, installs binary.
# Usage: curl -sSfL https://raw.githubusercontent.com/MFZNK05/DFS-Go/main/install.sh | sh

set -e

REPO="MFZNK05/DFS-Go"
BINARY="hermod"

# --- helpers ----------------------------------------------------------------

log()  { printf "\033[1;34m==>\033[0m %s\n" "$*"; }
err()  { printf "\033[1;31merror:\033[0m %s\n" "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || err "'$1' is required but not found"
}

# download URL to stdout using curl or wget
fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -sSfL "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$1"
  else
    err "curl or wget is required"
  fi
}

# download URL to file
download() {
  if command -v curl >/dev/null 2>&1; then
    curl -sSfL -o "$2" "$1"
  else
    wget -qO "$2" "$1"
  fi
}

# --- detect OS and arch -----------------------------------------------------

detect_os() {
  case "$(uname -s)" in
    Linux*)  echo "linux"  ;;
    Darwin*) echo "darwin" ;;
    *)       err "unsupported OS: $(uname -s) — download manually from https://github.com/$REPO/releases" ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)       echo "amd64" ;;
    aarch64|arm64)      echo "arm64" ;;
    *)                  err "unsupported architecture: $(uname -m)" ;;
  esac
}

# --- main -------------------------------------------------------------------

main() {
  OS="$(detect_os)"
  ARCH="$(detect_arch)"

  log "Detected $OS/$ARCH"

  # Fetch latest release tag
  log "Fetching latest release..."
  TAG="$(fetch "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
  [ -z "$TAG" ] && err "could not determine latest release"
  VERSION="${TAG#v}"
  log "Latest version: $VERSION"

  # Build download URL
  ARCHIVE="${BINARY}_${VERSION}_${OS}_${ARCH}.tar.gz"
  URL="https://github.com/$REPO/releases/download/$TAG/$ARCHIVE"
  CHECKSUMS_URL="https://github.com/$REPO/releases/download/$TAG/checksums.txt"

  # Create temp directory and clean up on exit
  TMPDIR="$(mktemp -d)"
  trap 'rm -rf "$TMPDIR"' EXIT

  # Download archive and checksums
  log "Downloading $ARCHIVE..."
  download "$URL" "$TMPDIR/$ARCHIVE"
  download "$CHECKSUMS_URL" "$TMPDIR/checksums.txt"

  # Verify checksum
  log "Verifying checksum..."
  EXPECTED="$(grep "$ARCHIVE" "$TMPDIR/checksums.txt" | awk '{print $1}')"
  if [ -n "$EXPECTED" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      ACTUAL="$(sha256sum "$TMPDIR/$ARCHIVE" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      ACTUAL="$(shasum -a 256 "$TMPDIR/$ARCHIVE" | awk '{print $1}')"
    else
      log "Warning: no sha256sum or shasum found, skipping checksum verification"
      ACTUAL="$EXPECTED"
    fi
    [ "$ACTUAL" != "$EXPECTED" ] && err "checksum mismatch: expected $EXPECTED, got $ACTUAL"
    log "Checksum OK"
  else
    log "Warning: checksum not found in checksums.txt, skipping verification"
  fi

  # Extract
  tar -xzf "$TMPDIR/$ARCHIVE" -C "$TMPDIR"

  # Install
  if [ -w /usr/local/bin ]; then
    INSTALL_DIR="/usr/local/bin"
  else
    INSTALL_DIR="$HOME/.local/bin"
    mkdir -p "$INSTALL_DIR"
  fi

  mv "$TMPDIR/$BINARY" "$INSTALL_DIR/$BINARY"
  chmod +x "$INSTALL_DIR/$BINARY"

  log "Installed $BINARY to $INSTALL_DIR/$BINARY"

  # Check PATH
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) printf "\n\033[1;33mWarning:\033[0m %s is not in your PATH.\n" "$INSTALL_DIR"
       printf "Add it with: export PATH=\"%s:\$PATH\"\n\n" "$INSTALL_DIR" ;;
  esac

  log "$("$INSTALL_DIR/$BINARY" version 2>/dev/null || echo "$BINARY $VERSION")"
  log "Installation complete! Run '$BINARY' to get started."
}

main
