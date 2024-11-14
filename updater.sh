#!/bin/sh

wget "https://github.com/nshebang/neosaki/releases/latest/download/neosaki" \
	-O "neosaki.1"

[ -f "neosaki.1" ] && \
	rm -f neosaki && \
	mv "neosaki.1" neosaki && \
	chmod +x neosaki && \
	echo "[OK] Update applied!"
