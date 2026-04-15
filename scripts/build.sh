#!/bin/bash
set -e

echo "Building Lambda binaries (arm64)..."

for cmd in fetcher enricher api migrate; do
  echo "  Building $cmd..."
  GOARCH=arm64 GOOS=linux go build \
    -tags lambda.norpc \
    -o bootstrap \
    "./cmd/$cmd"
  zip -q "$cmd.zip" bootstrap
  rm bootstrap
  echo "  → $cmd.zip"
done

echo "Done."
