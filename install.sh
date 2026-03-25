#!/bin/bash
set -e

REPO="Chetas-Patil/terminalformac"
BINARY_NAME="tfm"
INSTALL_PATH="/usr/local/bin/$BINARY_NAME"

echo "Installing $BINARY_NAME..."

if command -v go >/dev/null 2>&1; then
    echo "Go is installed, building from source..."
    go build -o $BINARY_NAME main.go
    sudo mv $BINARY_NAME $INSTALL_PATH
    echo "Installation complete: $BINARY_NAME is now at $INSTALL_PATH"
else
    echo "Go is not installed. Please install Go or download a pre-built binary from the GitHub releases page."
    exit 1
fi
