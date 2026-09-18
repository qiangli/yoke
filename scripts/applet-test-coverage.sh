#!/bin/sh
# Fail when a package shipped through cmds/all has no package-local Go test.
set -eu

root=$(git rev-parse --show-toplevel)
cd "$root"

failed=
packages=$(sed -n 's#.*github.com/qiangli/yoke/cmds/\([^"/]*\)"#\1#p' cmds/all/all.go | sort -u)
for package in $packages; do
	set -- "cmds/$package/"*_test.go
	if [ ! -f "$1" ]; then
		echo "applet-test-coverage: cmds/$package is shipped but has no package-local test" >&2
		failed="$failed $package"
	fi
done

if [ -n "$failed" ]; then
	echo "applet-test-coverage: FAIL —$failed" >&2
	exit 1
fi
echo "applet-test-coverage: PASS ($(printf '%s\n' $packages | wc -l | tr -d ' ') shipped packages)"
