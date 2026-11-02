#!/bin/sh
# Build Metis and its tui-dash compatibility command without VCS stamping.
# See Makefile for why -buildvcs=false is needed on pop-os.
set -e
cd "$(dirname "$0")"
GOCACHE=/tmp/td-gocache go build -buildvcs=false -o metis .
ln -sf metis tui-dash
echo "built ./metis (compatibility command: ./tui-dash)"
