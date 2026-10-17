#!/bin/bash
set -e

cd /opt/hermes

if [ -f ".venv/bin/activate" ]; then
  source .venv/bin/activate
  chown -R hermes:hermes /opt/data 2>/dev/null || true
  chmod 755 /opt/data/.env
  if [ -f "/opt/data/auth.json" ]; then
    chmod 664 /opt/data/auth.json 2>/dev/null || true
  fi

  hermes config set model.provider "custom"
  hermes config set model.base_url "http://172.17.0.1:9120/v1"
  hermes config set model.default "swift-qwen3.8"
  hermes config set OPENAI_API_KEY "no-key-required"
  hermes config set model_aliases.swift-qwen3.8 "custom/swift-qwen3.8"
  exec hermes gateway run
fi

