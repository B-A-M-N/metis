#!/bin/sh
# Build tui-dash without VCS stamping.
# See Makefile for why -buildvcs=false is needed on pop-os.
set -e
cd "$(dirname "$0")"
go build -buildvcs=false -o tui-dash .
echo "built ./tui-dash"
