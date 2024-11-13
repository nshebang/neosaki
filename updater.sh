#!/bin/sh

wget "https://github.com/nshebang/neosaki/releases/latest/download/neosaki" \	-O neosaki

[ -f neosaki ] && chmod +x neosaki && rm -f "neosaki.1" && \
echo "[OK] Update applied!"

