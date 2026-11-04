# Native version 0.1.1 validation

## Browser renewal follow-up (2026-11-04)

PR #2 adds browser-token renewal through CPA's refresh scheduler. The merged
implementation preserves enabled/disabled account settings and lets CPA retain
the existing account filename and label. An empty, malformed, expiry-less or
expired replacement token, `shouldLogout`, or a rejected refresh grant preserves
the original credentials and persists `cursor_auth.refreshStopped`. It clears
the refresh interval and prevents further renewal calls after reload/restart.
The existing access token remains subject to its original expiry; signing in
again creates credentials without the stop marker. Temporary server failures
and rate limits remain errors eligible for later refresh attempts.

Local validation on macOS ARM64 with Go 1.26.0: `go test -race ./...`, `go vet
./...`, shared-library build, and the C ABI smoke test passed. Regression tests
cover enabled and disabled accounts with fresh/due tokens, preservation of
account metadata, invalid renewal responses, restart persistence, no subsequent
renewal requests, original-token acceptance before expiry and rejection at
expiry, and temporary server failures. Tests use synthetic credentials and host
HTTP fixtures; no real Cursor login was used for this follow-up.

## Store audit follow-up (2026-10-22)

PR #277 was closed because the release lacked Darwin AMD64 and ARM64 binaries.
The release matrix now includes native Intel and Apple Silicon macOS runners,
with the same Go checks, C ABI smoke test and ZIP verification as other targets.
Packaging verifies Mach-O architecture and dynamic-library type. Full-release
checks and store submission require all five platform archives. The submission
script uses a new branch to avoid reusing the closed PR's existing branch.

Local validation: nine publishing tests passed, including rejection of a
release that has Linux/Windows archives but lacks Darwin, missing-platform
checks for all five targets, and invalid Mach-O architecture/type checks.
Workflow YAML and submission preview were checked. A cached Go 1.26.0 toolchain
was located outside PATH: Go race tests and go vet passed. The macOS ARM64
dynamic library built successfully, passed the C ABI smoke test, and packaged
as `spx-cursor_0.1.1_darwin_arm64.zip` with Mach-O verification.
The complete five-platform release still needs to be built and published by CI
before submitting a replacement store PR. Version 0.1.1 leaves the existing
0.1.0 tag and release intact. Publishing is currently blocked by invalid local
GitHub credentials (API 401 and Git push dry-run authentication failure).

## Execution format compatibility

Execution format compatibility: CPA normalizes Chat Completions to `openai`.
The executor accepts that canonical name and Chat Completions aliases. Host HTTP
fixtures cover streaming and non-streaming execution with both `openai` and
`chat-completions`; other protocol payloads remain rejected by the plugin.

The initial release includes the Cursor display name, the official Cursor app icon,
and a plugin resource page for API key login. The page calls a CPA-authenticated
management route; keys are validated through the CPA HTTP callback and saved via
`host.auth.save`. Synthetic tests cover successful persistence, invalid input,
upstream rejection, network/server errors, and persistence failures. The C ABI
smoke test also checks management registration and HTML resource rendering.
Real Cursor credentials and a deployed CPA management UI still require testing.

## Earlier validation history

Store publishing support added on 2026-10-21:

- Go race tests, `go vet`, and the updated native C ABI smoke test passed locally.
- Six publishing tests passed: release completeness, checksum tampering, ZIP path
  rejection, architecture mismatch, registry append/duplicate checks, and refusing
  to submit a store PR when release assets are missing.
- Workflow YAML, Bash syntax and Python syntax were checked locally.
- Linux AMD64/ARM64 and Windows AMD64 builds are configured in GitHub Actions.
  They have not yet run; their native libraries are not locally built or verified.
  No macOS release archive is configured.
- No GitHub Release or official store PR has been created by this change.
  Shell access to GitHub fails DNS resolution in the current environment.

Environment: macOS ARM64, Go 1.26.0. There are no third-party Go modules.

Passed:

- `go test -race ./...`: 932 test/subtest cases, including 864 model resolution
  cases compared with checked-in expectations from the MIT reference.
- Conversation parity: system/developer prompts, split tool calls, missing
  results, reasoning, inline images and tool-result content.
- Protobuf value parity, malformed argument rejection, compressed Connect
  frames, protocol error status mapping, and seeded binary parser fuzz cases.
- In-process CPA HTTP fixtures exercise real native RunSSE/BidiAppend,
  content-addressed transcript blob requests, tool call delivery, rebuilding
  history with a real client tool result, and final model output.
- HTTP response-head and silent-read cancellation, callback context propagation,
  status preservation, concurrent account isolation, API key exchange caching,
  PKCE browser login, expiry, quota windows and on-demand team pool behavior.
- An intermittent error-path cleanup race found during migration was fixed.
  Cancellation and error cleanup tests then passed 20 consecutive race runs.
- `go vet ./...`.
- `go build -buildmode=c-shared -o dist/cursor.dylib .`.
- The compiled native test binary passed with `PATH=/nonexistent` and
  `CURSOR_PLUGIN_NODE=/no-node`.
- The actual library loaded through C ABI v1 via Python ctypes under that same
  environment. Registration, credential parsing, native quota HTTP callbacks,
  error status mapping and callback buffer ownership all passed. Python was
  the test host only; it is not used by the library.
- `go list -deps .` contains neither `os/exec` nor third-party module packages.
  `otool -L` shows only macOS system libraries/frameworks, not libnode or a JS
  engine. macOS Keychain access uses Security/CoreFoundation directly.
- The portable Go core compiled as test binaries for Linux AMD64, Linux ARM64
  and Windows AMD64 using `CGO_ENABLED=0`. Those binaries were not executed;
  this is a core compile check, not a native C ABI build or runtime validation
  for those platforms.

The standard `make test` workflow does not invoke Node, Bun or Python.
`testdata/reference-golden.json` is checked in. The JS reference and optional
fixture regeneration script are developer resources, excluded from the native
library and binary release archive.

No real Cursor credentials were used. Real subscription access, actual proxy
traffic, the CPA frontend and full CPA server routing remain deployment checks.
The underlying host integration continues to use CPA's existing HTTP callbacks
for its global proxy and logging. Per-account proxy overrides still require a
host API enhancement and fail explicitly in this plugin.

This version uses complete Chat history for tool continuations, matching the
prior request-scoped CPA transport. It does not implement the reference's
cross-request native Run retention or deferred dashboard token ledger. Missing
upstream step counters are explicitly marked as estimated; final TurnEnded
counters are preserved.
