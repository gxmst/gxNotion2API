#!/usr/bin/env bash
# Run on the systemd host with the installation root and health base URL set.
# Example: NOTION2API_ROOT=/path/to/install NOTION2API_URL=http://host:port \
#   bash scripts/deploy-release.sh VERSION COMMIT SHA256
set -Eeuo pipefail
version=${1:?version required}
commit=${2:?commit required}
expected_sha=${3:?release SHA-256 required}
base=${NOTION2API_ROOT:?installation root required}
base_url=${NOTION2API_URL:?service base URL required}
service=${NOTION2API_SERVICE:-notion2api}
repository=${NOTION2API_REPOSITORY:-gxmst/gxNotion2API}
[[ "$version" =~ ^v[0-9][A-Za-z0-9.-]*$ ]]
[[ "$commit" =~ ^[0-9a-f]{40}$ && "$expected_sha" =~ ^[0-9a-f]{64}$ ]]
base=$(realpath "$base")
base_url=${base_url%/}
asset=notion2api_${version}_linux_amd64.tar.gz
stamp=$(TZ=Asia/Shanghai date +%Y%m%dT%H%M%S%z)
release=$base/releases/${version}-${stamp}-${commit:0:7}
backup=$base/backups/pre-${version}-${stamp}-${commit:0:7}
test -f "$base/bin/notion2api"
test -d "$base/static/admin"
mkdir -m 755 "$release"
curl --fail --location --connect-timeout 15 --max-time 180 --output "$release/$asset" "https://github.com/$repository/releases/download/$version/$asset"
printf '%s  %s\n' "$expected_sha" "$release/$asset" | sha256sum -c -
python3 - "$release/$asset" "notion2api_${version}_linux_amd64" <<'PY'
import sys, tarfile
from pathlib import PurePosixPath
with tarfile.open(sys.argv[1]) as archive:
    for entry in archive.getmembers():
        p = PurePosixPath(entry.name)
        if p.is_absolute() or '..' in p.parts or p.parts[0] != sys.argv[2] or not (entry.isfile() or entry.isdir()):
            raise SystemExit('unexpected release archive entry')
        if any(part in {'data', 'probe_files', 'node_modules', '.next', 'dist'} for part in p.parts) or p.suffix in {'.db', '.sqlite', '.log', '.pdb'}:
            raise SystemExit('non-deliverable files in release archive')
PY
tar -xzf "$release/$asset" -C "$release"
package=$release/notion2api_${version}_linux_amd64
test -x "$package/notion2api"
test -s "$package/static/admin/index.html"
# Boot the actual packaged executable with disposable data before stopping production.
python3 - "$package" <<'PY'
import json, secrets, socket, subprocess, sys, tempfile, time, urllib.request, urllib.error
from pathlib import Path
package = Path(sys.argv[1])
with tempfile.TemporaryDirectory(prefix='notion2api-release-smoke-') as tmp:
    root = Path(tmp)
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
    cfg = json.loads((package/'config.example.json').read_text())
    cfg.update(host='127.0.0.1', port=port, api_key=secrets.token_hex(24), accounts=[], active_account='', probe_json=str(root/'missing-probe.json'))
    cfg['admin'].update(password=secrets.token_hex(24), static_dir=str(package/'static/admin'))
    cfg['session_refresh'].update(enabled=False, startup_check=False)
    cfg['storage']['sqlite_path'] = str(root/'smoke.sqlite')
    path = root/'config.json'; path.write_text(json.dumps(cfg))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    proc = subprocess.Popen([str(package/'notion2api'), '--config', str(path)], cwd=root, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    try:
        url = f'http://127.0.0.1:{port}'
        for _ in range(40):
            if proc.poll() is not None: raise RuntimeError('packaged executable exited during smoke test: '+proc.stderr.read().decode()[:2000])
            try:
                if json.load(opener.open(url+'/healthz', timeout=1)).get('ok'): break
            except (OSError, ValueError): pass
            time.sleep(.25)
        else: raise RuntimeError('isolated health check timed out')
        assert opener.open(url+'/admin/', timeout=3).read() == (package/'static/admin/index.html').read_bytes()
        try:
            opener.open(urllib.request.Request(url+'/admin/conversations/smoke/messages/message', method='DELETE'), timeout=3)
            raise RuntimeError('unauthenticated delete accepted')
        except urllib.error.HTTPError as error:
            assert error.code == 401
    finally:
        proc.terminate()
        try: proc.wait(timeout=10)
        except subprocess.TimeoutExpired: proc.kill(); proc.wait()
print('Isolated executable smoke test passed')
PY
mkdir -m 700 "$backup"
cp -a "$base/bin/notion2api" "$backup/notion2api"
cp -a "$base/static/admin" "$backup/admin"
unit_path=$(systemctl show "$service" -p FragmentPath --value)
cp -a "$unit_path" "$backup/service-unit"
install -m 755 "$package/notion2api" "$base/bin/notion2api.new-$stamp"
cp -a "$package/static/admin" "$base/static/admin.new-$stamp"
chmod -R a+rX "$base/static/admin.new-$stamp"
stopped=0
switched_ui=0
rollback() {
    result=$?
    trap - ERR
    if [ "$stopped" = 1 ]; then
        systemctl stop "$service" || true
        install -m 755 "$backup/notion2api" "$base/bin/notion2api.rollback-$stamp"
        mv -f "$base/bin/notion2api.rollback-$stamp" "$base/bin/notion2api"
        if [ "$switched_ui" = 1 ]; then
            mv "$base/static/admin" "$base/static/admin.failed-$stamp"
            mv "$base/static/admin.before-$stamp" "$base/static/admin"
        elif [ ! -d "$base/static/admin" ] && [ -d "$base/static/admin.before-$stamp" ]; then
            mv "$base/static/admin.before-$stamp" "$base/static/admin"
        fi
        systemctl start "$service" || true
    fi
    printf 'Deployment failed; rollback attempted. Backup: %s\n' "$backup" >&2
    exit "$result"
}
trap rollback ERR
systemctl stop "$service"
stopped=1
cp -a "$base/config" "$base/data" "$base/probe_files" "$backup/"
mv -f "$base/bin/notion2api.new-$stamp" "$base/bin/notion2api"
mv "$base/static/admin" "$base/static/admin.before-$stamp"
mv "$base/static/admin.new-$stamp" "$base/static/admin"
switched_ui=1
systemctl start "$service"
python3 - "$base_url" "$base/static/admin" <<'PY'
import json, re, sys, time, urllib.request
from pathlib import Path
base, ui = sys.argv[1], Path(sys.argv[2])
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
for _ in range(30):
    try:
        health = json.load(opener.open(base+'/healthz', timeout=2))
        if health.get('ok') and health.get('session_ready'): break
    except (OSError, ValueError): pass
    time.sleep(1)
else: raise RuntimeError('production health check failed')
html = opener.open(base+'/admin/', timeout=10).read()
assert html == (ui/'index.html').read_bytes(), 'UI mismatch'
assets = set(re.findall(r'(?:src|href)="([^" ]+\.(?:js|css))"', html.decode()))
assert assets
for asset in assets:
    if asset.startswith('/admin/'):
        assert opener.open(base+asset, timeout=10).read() == (ui/asset[len('/admin/'):]).read_bytes(), 'asset mismatch'
print('Production health, UI and asset checks passed')
PY
systemctl is-active --quiet "$service"
cmp "$package/notion2api" "$base/bin/notion2api"
cmp "$base/config/config.json" "$backup/config/config.json"
printf '%s  %s\n' "$expected_sha" "$release/$asset" | sha256sum -c -
printf 'version=%s\ncommit=%s\nsha256=%s\nbackup=%s\n' "$version" "$commit" "$expected_sha" "$backup" > "$release/DEPLOYMENT.txt"
trap - ERR
printf 'DEPLOYED=%s\nBACKUP=%s\n' "$release" "$backup"
systemctl show "$service" -p ActiveState -p SubState -p MainPID -p NRestarts
