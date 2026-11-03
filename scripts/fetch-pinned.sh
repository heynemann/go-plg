#!/usr/bin/env bash
# fetch-pinned.sh URL SHA256 DEST: downloads a pinned archive (.tar.gz or .zip), checks its SHA-256 and unpacks it
# into the directory DEST. A tarball's single top-level directory becomes DEST; a zip's files land in DEST. The
# archive is unpacked next to DEST and renamed into place, so concurrent builds never see a half-unpacked tree.
set -euo pipefail

url=$1 sum=$2 dest=$3

if [[ -d $dest ]]; then
	exit 0
fi

parent=$(dirname "$dest")
mkdir -p "$parent"
work=$(mktemp -d "$parent/.fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT

archive="$work/$(basename "$url")"
curl --fail --silent --show-error --location --retry 3 --output "$archive" "$url"
echo "$sum  $archive" | sha256sum --check --quiet

mkdir "$work/unpacked"
case $archive in
*.tar.gz) tar -xzf "$archive" -C "$work/unpacked" --strip-components=1 ;;
*.zip) unzip -q "$archive" -d "$work/unpacked" ;;
*) echo "fetch-pinned.sh: unknown archive type: $archive" >&2 && exit 1 ;;
esac

# Another build may have won the race; its tree is the same.
mv -T "$work/unpacked" "$dest" 2>/dev/null || test -d "$dest"
