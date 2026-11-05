#!/bin/sh
# Generate shell completions for packaging (run by GoReleaser before builds).
set -e
rm -rf completions
mkdir completions
for sh in bash zsh fish; do
    go run ./cmd/pathgauge completion "$sh" > "completions/pathgauge.$sh"
done
