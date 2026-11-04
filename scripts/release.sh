#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  echo '用法：./scripts/release.sh [v1.0.7]'
  echo '省略版本号时，自动取现有最新 tag 的下一个补丁版本。'
  echo '将当前已推送的 main 提交打 tag 并推送；GitHub Actions 自动检查、构建并创建同名 Release。'
}
fail() { echo "错误：$*" >&2; exit 1; }

if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then usage; exit 0; fi
[[ $# -le 1 ]] || { usage >&2; exit 1; }
[[ "$(git branch --show-current)" == main ]] || fail '请切换到 main 分支。'
[[ -z "$(git status --porcelain)" ]] || fail '工作区有未提交改动；请先提交或收起改动。'
git fetch --quiet origin main --tags
sha=$(git rev-parse HEAD)
[[ "$sha" == "$(git rev-parse refs/remotes/origin/main)" ]] || fail '当前 HEAD 尚未推送到 origin/main，或本地 main 未更新。'

if [[ $# == 1 ]]; then
  version=$1
else
  latest=''
  while IFS= read -r tag; do
    if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then latest=$tag; break; fi
  done < <(git tag --list 'v*' --sort=-version:refname)
  [[ -n "$latest" ]] || fail '没有现有版本 tag，请显式指定首个版本。'
  [[ "$latest" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]
  version="v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((10#${BASH_REMATCH[3]} + 1))"
fi
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail '版本格式应为 v1.0.7。'
[[ -z "$(git tag --list "$version")" ]] || fail "版本 $version 已存在，请使用新版本号。"

echo "发布版本：$version"
echo "对应提交：$sha"
git tag -a "$version" -m "Release $version"
git push origin "refs/tags/$version:refs/tags/$version"
repo=$(git remote get-url origin | sed -E 's#^git@github.com:#https://github.com/#; s#\.git$##')
echo "已推送 ${version}；发布进度：$repo/actions/workflows/release.yml"
echo 'tag 推送成功不代表发布已完成，请等待工作流成功后查看同名 GitHub Release。'
