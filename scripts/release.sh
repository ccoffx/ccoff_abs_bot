#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

usage() {
  echo '用法：./scripts/release.sh v1.0.1'
  echo '发布当前 HEAD；执行前须推送到 main、等待 CI 成功，并完成测试 Bot 与数据库验收。'
}
fail() { echo "错误：$*" >&2; exit 1; }

if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then usage; exit 0; fi
[[ $# == 1 ]] || { usage >&2; exit 1; }
version=$1
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail '版本格式应为 v1.0.1。'
command -v gh >/dev/null 2>&1 || fail '请先安装 GitHub CLI：brew install gh，然后执行 gh auth login。'
gh auth status >/dev/null 2>&1 || fail 'GitHub CLI 未登录，请执行 gh auth login。'
[[ -z "$(git status --porcelain)" ]] || fail '工作区有未提交改动；请先提交或使用 git stash 收起改动，确保验收代码与发布提交一致。'

sha=$(git rev-parse HEAD)
repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
# 查询失败也必须终止，不能把权限或网络错误当成版本不存在。
existing=$(gh api "repos/$repo/git/matching-refs/tags/$version" \
  --jq ".[] | select(.ref == \"refs/tags/$version\") | .ref")
[[ -z "$existing" ]] || fail "版本 $version 已存在，请使用新版本号。"
run_id=$(gh run list --repo "$repo" --workflow ci.yml --commit "$sha" \
  --branch main --event push --status success \
  --json databaseId --jq '.[0].databaseId // empty')
[[ -n "$run_id" ]] || fail '当前提交尚无 main 分支成功的候选 CI。请先推送并等待完整 CI 成功。'

echo "仓库：$repo"
echo "版本：$version"
echo "提交：$sha"
echo "候选 CI：https://github.com/$repo/actions/runs/$run_id"

gh workflow run release.yml --repo "$repo" --ref main \
  -f "commit=$sha" -f "version=$version" \
  -f private_chat_tested=true -f database_compatible=true
echo "已提交发版任务（尚未表示发布成功）：https://github.com/$repo/actions/workflows/release.yml"
echo "终端查看进度：gh run list --repo $repo --workflow release.yml --limit 5"
echo "跟踪某次任务：gh run watch <任务ID> --repo $repo --exit-status"
