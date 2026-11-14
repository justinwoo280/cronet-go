#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."
artifacts=${CRONET_ANDROID_ARTIFACT_DIR:-test-artifacts/android}
mkdir -p "$artifacts"
remote=/data/local/tmp/cronet-browser-tests
adb root
adb wait-for-device
adb shell "mkdir -p $remote"
adb push cronet-android.test "$remote/cronet.test"
adb shell "chmod 755 $remote/cronet.test"
adb logcat -b all -c
collect() {
  adb logcat -b all -d > "$artifacts/logcat.txt" 2>&1 || true
  adb pull /data/tombstones "$artifacts/tombstones" > "$artifacts/tombstones.log" 2>&1 || true
}
trap collect EXIT
adb shell getprop ro.build.version.sdk > "$artifacts/android-api.txt"

# Missing ECH configuration returns before the crashing resolver path. Ordinary
# Browser connections also provide a control for archive/linker/emulator issues.
adb shell "cd $remote && TMPDIR=$remote ./cronet.test -test.v -test.timeout=90s -test.run='^(TestBrowserECHLookupFailure|TestBrowserXHTTPNativeRoundTrip)$'" \
  > "$artifacts/control.log" 2>&1
cat "$artifacts/control.log"

if [[ ${CRONET_ANDROID_EXPECT_CRASH:-0} == 1 ]]; then
  set +e
  adb shell "cd $remote && TMPDIR=$remote ./cronet.test -test.v -test.timeout=90s -test.run='^TestBrowserECHLookupRecovery$'" \
    > "$artifacts/ech.log" 2>&1
  result=$?
  set -e
  collect
  cat "$artifacts/ech.log"
  if [[ $result == 0 ]] || ! grep -Eq 'SIGSEGV|Fatal signal 11|signal arrived during cgo execution' "$artifacts/ech.log" "$artifacts/logcat.txt"; then
    echo "Expected the unfixed Android native ECH crash; exit status was $result" >&2
    exit 1
  fi
  echo "Reproduced native ECH crash after successful lookup; controls passed."
else
  adb shell "cd $remote && TMPDIR=$remote ./cronet.test -test.v -test.timeout=5m -test.run='^(TestBrowserECHLookupRecovery|TestBrowserStrictECH|TestBrowserStrictECHReuse|TestStrictECHEnginePolicy)$'" \
    > "$artifacts/ech.log" 2>&1
  cat "$artifacts/ech.log"
fi
