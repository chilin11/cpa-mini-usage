#!/usr/bin/env bash
set -euo pipefail
# Run once in this directory after authenticating with GitHub CLI (gh auth login).
command -v gh >/dev/null || { echo 'Install GitHub CLI (gh) first.' >&2; exit 1; }
gh auth status >/dev/null
OWNER="$(gh api user --jq .login)"
OWNER="$OWNER" python3 - <<'PY'
import json,os,pathlib
owner=os.environ['OWNER']
base=pathlib.Path('.')
p=base/'registry.json'
obj=json.loads(p.read_text())
for plug in obj['plugins']:
    plug['repository']=f'https://github.com/{owner}/cpa-mini-usage'
    plug['homepage']=plug['repository']
p.write_text(json.dumps(obj,indent=2,ensure_ascii=False)+'\n')
p=base/'main.go'
s=p.read_text().replace('github.com/your-name/cpa-mini-usage',f'github.com/{owner}/cpa-mini-usage')
p.write_text(s)
p=base/'go.mod'
s=p.read_text().replace('github.com/your-name/cpa-mini-usage',f'github.com/{owner}/cpa-mini-usage')
p.write_text(s)
PY

git init -b main
git add .
git commit -m 'Initial CPA Mini Usage plugin' || true
gh repo create "${OWNER}/cpa-mini-usage" --public --source=. --remote=origin --push

git tag v0.1.0
git push origin v0.1.0
printf '\nPublished: https://github.com/%s/cpa-mini-usage\n' "$OWNER"
