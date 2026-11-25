#!/usr/bin/env bash
# Resume interrupted uploads and verify all bytes before publishing a draft.
set -euo pipefail
: "${VERSION:?}" "${REPO:?}" "${SHA:?}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
view() { gh release view "$VERSION" --repo "$REPO" --json isDraft,assets > "$work/state.json"; }
if ! view 2> "$work/error"; then
  if ! grep -q '^release not found$' "$work/error"; then
    cat "$work/error" >&2
    exit 1
  fi
  gh release create "$VERSION" --repo "$REPO" --verify-tag --target "$SHA" \
    --title "$VERSION" --notes-file dist/notes.md --draft
  view
fi
draft=$(jq -r .isDraft "$work/state.json")
for file in dist/deployment.tar.gz dist/deployment.tar.gz.sha256 dist/release.json; do
  name=${file##*/}
  if ! jq -e --arg name "$name" '.assets | any(.name == $name)' "$work/state.json" > /dev/null; then
    if [[ "$draft" != true ]]; then
      echo "Published release is missing asset: $name; refusing to modify it" >&2
      exit 1
    fi
    # Sequential uploads avoid concurrent attachment races. A failed response
    # can still mean the server accepted the bytes; verify by downloading below.
    gh release upload "$VERSION" --repo "$REPO" "$file" || echo "Upload response failed; checking stored asset: $name" >&2
  fi
  gh release download "$VERSION" --repo "$REPO" --pattern "$name" --dir "$work"
  if ! cmp -s "$file" "$work/$name"; then
    echo "Release asset differs: $name; refusing to overwrite it" >&2
    exit 1
  fi
done
if [[ "$draft" == true ]]; then
  gh release edit "$VERSION" --repo "$REPO" --draft=false
fi
echo "Verified published release: $VERSION"
