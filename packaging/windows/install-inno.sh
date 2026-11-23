#!/usr/bin/env bash
# Installs a pinned, checksum-verified Inno Setup 6 into C:\inno (CI on GitHub's Windows
# runners, Git Bash). build.sh then compiles the installer with ISCC=C:\inno\ISCC.exe.
set -euo pipefail

VERSION=6.7.4
SHA256=a549929319f9120716677c685535ca4671e4b937b5fdc87e2585f32d8eb30e8c
TAG="is-${VERSION//./_}"
EXE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/innosetup-$VERSION.exe"

curl -fsSL --retry 3 -o "$EXE" "https://github.com/jrsoftware/issrc/releases/download/$TAG/innosetup-$VERSION.exe"
echo "$SHA256  $EXE" | sha256sum -c -
MSYS2_ARG_CONV_EXCL='*' "$EXE" /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /SP- /CURRENTUSER '/DIR=C:\inno'
test -f /c/inno/ISCC.exe
echo "Inno Setup $VERSION installed in C:\\inno"
