#!/bin/bash
set -e

# ChatOps Master One-Click Installer for Linux amd64
# Usage: curl -fsSL https://raw.githubusercontent.com/AltProto-Studio/ChatOps/main/install-master.sh | sudo bash -s -- --token YOUR_TG_BOT_TOKEN

echo "=================================================="
echo "      ChatOps Master Node Installer (Linux amd64)  "
echo "=================================================="

# Check if running as root
if [ "$EUID" -ne 0 ]; then
  echo "❌ Error: Please run this script with sudo or as root."
  exit 1
fi

TG_TOKEN=""

# Parse arguments
while [[ "$#" -gt 0 ]]; do
  case $1 in
    --token) TG_TOKEN="$2"; shift ;;
    *) echo "Unknown parameter passed: $1"; exit 1 ;;
  esac
  shift
done

INSTALL_DIR="/opt/gopass"
mkdir -p "$INSTALL_DIR"

echo "📥 Downloading latest gopass-master binary from GitHub..."
URL="https://github.com/AltProto-Studio/ChatOps/releases/latest/download/gopass-master-linux-amd64"
if ! curl -L -o "$INSTALL_DIR/gopass-master" "$URL"; then
  echo "❌ Error: Failed to download binary. Please make sure the release is published."
  exit 1
fi
chmod +x "$INSTALL_DIR/gopass-master"

# Generate default master.yaml if it doesn't exist
CONFIG_PATH="$INSTALL_DIR/master.yaml"
if [ ! -f "$CONFIG_PATH" ]; then
  echo "📝 Generating default master.yaml..."
  cat <<EOF > "$CONFIG_PATH"
# GOPASS Master Configuration File
db_path: "$INSTALL_DIR/gopass-master.db"
grpc_addr: "0.0.0.0:50051"
telegram_token: "${TG_TOKEN:-YOUR_TELEGRAM_BOT_TOKEN_HERE}"
tls_enabled: false
tls_cert_path: ""
tls_key_path: ""
EOF
else
  if [ ! -z "$TG_TOKEN" ]; then
    echo "📝 Updating Telegram Token in master.yaml..."
    sed -i "s/telegram_token:.*/telegram_token: \"$TG_TOKEN\"/" "$CONFIG_PATH"
  fi
fi

# Create systemd service file
SERVICE_PATH="/etc/systemd/system/gopass-master.service"
echo "⚙️ Creating systemd service at $SERVICE_PATH..."
cat <<EOF > "$SERVICE_PATH"
[Unit]
Description=ChatOps Master Service
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/gopass-master -config $CONFIG_PATH
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

# Reload and start service
echo "🔄 Reloading systemd daemon and starting service..."
systemctl daemon-reload
systemctl enable gopass-master
systemctl restart gopass-master

echo "=================================================="
echo "🎉 ChatOps Master installation completed!"
echo "📍 Install Path: $INSTALL_DIR"
echo "📝 Configuration: $CONFIG_PATH"
echo "🔧 Service control: sudo systemctl status gopass-master"
echo "=================================================="
if [ -z "$TG_TOKEN" ] || [ "$TG_TOKEN" == "YOUR_TELEGRAM_BOT_TOKEN_HERE" ]; then
  echo "⚠️ Warning: Please edit $CONFIG_PATH to set your real Telegram Bot Token, then restart: sudo systemctl restart gopass-master"
fi
