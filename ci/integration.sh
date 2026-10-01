#!/usr/bin/env bash
# Integration test for upgrade-caddy's privileged paths: first install,
# in-place upgrade with a running systemd unit, file capabilities, rollback
# when the new binary cannot serve the live config, refusal over a system
# package, and a plain `sudo upgrade-caddy install` that builds as root.
#
# It writes a systemd unit, installs a distribution package and runs the tool
# as root. It is meant for a throwaway CI virtual machine with passwordless
# sudo and refuses to run unless UPGRADE_CADDY_CI=1 says that is where it is.
set -euo pipefail

if [[ "${UPGRADE_CADDY_CI:-}" != "1" ]]; then
    echo "refusing to run: this script changes the system; set UPGRADE_CADDY_CI=1 only on a throwaway VM" >&2
    exit 2
fi

TOOL=${TOOL:-$PWD/upgrade-caddy}
TARGET=/usr/local/bin/caddy
UNIT=caddy-ci.service
PORT=18080
PLUGIN=github.com/caddyserver/replace-response
OLD_VERSION=${OLD_VERSION:-v2.11.4}
WORK=$(mktemp -d)
CONF=/etc/caddy-ci

step() { printf '\n==> %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_eq() { [[ "$1" == "$2" ]] || fail "$3: got '$1', want '$2'"; }
assert_contains() { grep -q -- "$2" <<<"$1" || fail "$3: output does not contain '$2'"; }
version_of() { "$1" version | cut -d' ' -f1; }
main_exe() { sudo readlink "/proc/$(systemctl show -p MainPID --value "$UNIT")/exe"; }
serve_test() { curl -sf "http://127.0.0.1:$PORT/" || echo "<no response>"; }

# sudo with the runner's PATH, so the Go toolchain from setup-go is visible to root.
SUDO=(sudo env "PATH=$PATH" GOTOOLCHAIN=auto)

step "building test binaries as $(id -un)"
"$TOOL" build --fresh --caddy-version "$OLD_VERSION" --with "$PLUGIN" --output "$WORK/caddy-old"
"$TOOL" build --fresh --with "$PLUGIN" --output "$WORK/caddy-new"
"$TOOL" build --fresh --caddy-version "$OLD_VERSION" --output "$WORK/caddy-vanilla"
NEW_VERSION=$(version_of "$WORK/caddy-new")
echo "old=$OLD_VERSION new=$NEW_VERSION"

step "1. first install with --from into an empty target"
"${SUDO[@]}" "$TOOL" install --from "$WORK/caddy-old" --target "$TARGET"
assert_eq "$(version_of "$TARGET")" "$OLD_VERSION" "installed version"
[[ -f "$TARGET.lock.json" ]] || fail "lockfile missing"
[[ ! -e "$TARGET.previous" ]] || fail "no .previous expected on first install"

step "creating $UNIT with a config that needs the plugin"
sudo mkdir -p "$CONF"
sudo tee "$CONF/Caddyfile" >/dev/null <<CADDY
{
	admin off
	order replace after encode
}
:$PORT {
	respond "hello"
	replace hello world
}
CADDY
sudo tee "/etc/systemd/system/$UNIT" >/dev/null <<UNITFILE
[Unit]
Description=Caddy (upgrade-caddy integration test)
After=network.target

[Service]
Type=notify
ExecStart=$TARGET run --config $CONF/Caddyfile
Restart=no

[Install]
WantedBy=multi-user.target
UNITFILE
sudo systemctl daemon-reload
sudo systemctl start "$UNIT"
sleep 1
assert_eq "$(systemctl is-active "$UNIT")" "active" "unit after start"
assert_eq "$(serve_test)" "world" "response from old binary"

step "2. in-place upgrade with --from, unit running, file capabilities set"
sudo setcap cap_net_bind_service=+ep "$TARGET"
getcap "$TARGET"
OLD_INODE=$(stat -c %i "$TARGET")
OUT=$("${SUDO[@]}" "$TOOL" install --from "$WORK/caddy-new" --target "$TARGET" 2>&1) || { echo "$OUT"; fail "install exited non-zero"; }
echo "$OUT"
assert_contains "$OUT" "Validate: $CONF/Caddyfile (from $UNIT)" "config taken from the unit"
assert_contains "$OUT" "Restarted $UNIT" "unit restarted"
assert_eq "$(version_of "$TARGET")" "$NEW_VERSION" "target version after upgrade"
assert_eq "$(stat -c %i "$TARGET.previous")" "$OLD_INODE" ".previous is the old inode"
assert_eq "$(version_of "$TARGET.previous")" "$OLD_VERSION" ".previous version"
assert_eq "$(systemctl is-active "$UNIT")" "active" "unit after upgrade"
assert_eq "$(main_exe)" "$TARGET" "unit's main process runs the target"
assert_contains "$(getcap "$TARGET")" "cap_net_bind_service" "file capabilities re-applied"
assert_eq "$(serve_test)" "world" "response from new binary"
[[ -f "$TARGET.lock.json" ]] || fail "lockfile missing after upgrade"

step "3. rollback: a vanilla build validates against --config but cannot serve the unit's config"
printf ':18081 {\n\trespond "plain"\n}\n' >"$WORK/plain.Caddyfile"
set +e
OUT=$("${SUDO[@]}" "$TOOL" install --from "$WORK/caddy-vanilla" --target "$TARGET" --config "$WORK/plain.Caddyfile" 2>&1)
RC=$?
set -e
echo "$OUT"
assert_eq "$RC" "1" "install must fail when the unit does not come up"
assert_contains "$OUT" "rolled back" "rollback reported"
assert_eq "$(version_of "$TARGET")" "$NEW_VERSION" "target restored to the previous binary"
assert_eq "$(version_of "$TARGET.failed")" "$OLD_VERSION" "failed binary kept as .failed"
[[ ! -e "$TARGET.previous" ]] || fail ".previous should have been consumed by the rollback"
sleep 1
assert_eq "$(systemctl is-active "$UNIT")" "active" "unit active again after rollback"
assert_eq "$(main_exe)" "$TARGET" "unit runs the restored binary"
assert_eq "$(serve_test)" "world" "response after rollback"

step "4. plain 'sudo upgrade-caddy install': builds as root from the installed binary"
OUT=$("${SUDO[@]}" "$TOOL" install --target "$TARGET" --caddy-version "$OLD_VERSION" 2>&1) || { echo "$OUT"; fail "root build install exited non-zero"; }
echo "$OUT" | grep -v '^    '
assert_contains "$OUT" "$PLUGIN" "plugin set reproduced from the installed binary"
assert_eq "$(version_of "$TARGET")" "$OLD_VERSION" "version after root build install"
assert_eq "$(systemctl is-active "$UNIT")" "active" "unit after root build install"
assert_eq "$(serve_test)" "world" "response after root build install"

step "5. refusal over the distribution package"
if ! apt-cache show caddy >/dev/null 2>&1; then
    echo "this release has no caddy package; skipping"
    step "all integration checks passed"
    exit 0
fi
sudo apt-get install -y -qq caddy >/dev/null
PKG_INODE=$(stat -c %i /usr/bin/caddy)
set +e
OUT=$("${SUDO[@]}" "$TOOL" install --target /usr/bin/caddy 2>&1)
RC=$?
set -e
echo "$OUT"
assert_eq "$RC" "1" "install over a package must fail"
assert_contains "$OUT" "sudo apt remove caddy" "uninstall instruction"
assert_eq "$(stat -c %i /usr/bin/caddy)" "$PKG_INODE" "package binary untouched"
set +e
OUT=$("$TOOL" check --binary /usr/bin/caddy 2>&1)
RC=$?
set -e
echo "$OUT"
assert_eq "$RC" "2" "check on the package reports updates available"
assert_contains "$OUT" 'owned by the dpkg package "caddy"' "check names the package"

step "all integration checks passed"
