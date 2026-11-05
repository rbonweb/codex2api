# Codex real client version pairing

The app versions of Desktop and VSCode are synced and cached together with their bundled CLI versions. When the app version is upgraded, the CLI may stay the same, be downgraded or carry an `alpha` suffix; a pairing that does not exist must not be assembled from the latest stable CLI.

## Sources and targets

| Client | Target | Preferred source | Verification and fallback |
| --- | --- | --- | --- |
| Desktop macOS | `darwin-arm64`, `darwin-x64` | Official appcast for the matching architecture | Read the outer `Contents/Info.plist` and `codex-cli/codex-package.json` of the ZIP with Range requests; check the app, build number and CLI architecture |
| Desktop Windows | `win32-x64`, `win32-arm64` | `https://codexapp.agentsmirror.com/latest/manifest` | The mapping must match the package identity, package version and architecture in the official Windows update manifest; on failure, read the MSIX from the official download address |
| VSCode | macOS, Windows, Linux, each with x64/ARM64 | Latest stable `openai.chatgpt` on the official Marketplace | Read the extension version and CLI manifest of the matching native VSIX with Range requests; the Linux/WSL CLI inside the Windows package does not count as the Windows version |
| TUI / exec | Standalone CLI | Latest stable release on official GitHub | Keep the original CLI sync logic, independent of the Desktop / VSCode pairing |

Choosing a stable Desktop/extension package does not mean its bundled CLI must be stable. The full CLI version in the bundled manifest or binary (for example `0.158.0-alpha.2.1`) is kept as is.

## Windows official fallback

An existing MSIX may not contain `codex-package.json`, so "manifest does not exist" must not be treated as meaning that all official packages are unsupported. The fallback flow is as follows:

First read the official versioned download address; only when it returns 404, fall back to the official latest package address for the matching architecture. The latest package may lag behind the Store manifest, so the upstream major/minor version check is kept, and the actual version inside the package and the full pairing are saved. A mutable download address is identified by URL, ETag, Last-Modified and size; when the ETag is unchanged, only one byte needs to be probed again, and the actual package version is rechecked against the current Store manifest, so the pairing cache can be reused.

1. Get the ZIP/ZIP64 directory, read `AppxManifest.xml`, and check `OpenAI.Codex`, the official package version and the target architecture.
2. If the package contains a CLI manifest, use the manifest; without a manifest, read `AppxBlockMap.xml`. A damaged manifest or one with a mismatched architecture is not ignored.
3. Use the compressed size of each 64 KiB raw block in the block map to locate the byte range, decompress the required blocks independently, and check the raw length and SHA-256. A non-final block has no DEFLATE end marker, so the decompressed `UnexpectedEOF` is accepted only when both the length and the hash are correct.
4. Read the header of `app.asar` and the block that holds the root `package.json`, skip the middle content directly, and get the real desktop build number.
5. Read the PE header of `codex.exe`, check x64/ARM64, locate the read-only `.rdata`, and read at most 8 MiB of raw data in batches. The CLI build version must appear in both the `codex-doctor/<version>` and `version: <version>\nplatform: ` fixed strings, and the versions must match. The downloaded binary is never executed.

If the compression format, block index, hash or version string is not supported, only a failure status is returned and nothing is written to the database; the last fully successful pairing continues to be used. When the first sync has no cached pairing, the built-in pairing explicitly marked `builtin_observed` is used; the code does not switch to an arbitrary latest CLI and does not keep downloading the full package.

The read budget applies to each installer package separately: at most **12 MiB of Range response body and 64 requests**, with a cache block size of 256 KiB. The server must return an exactly matching `206`, `Content-Range`, body length and stable ETag / Last-Modified; if it returns the whole package with `200`, reading stops immediately. Only a well-formed strong ETag is sent as `If-Range`, which is compatible with the unquoted ETag of the Marketplace. Plain small files are limited to 64 KiB, and the block map and ASAR header are each limited to 8 MiB.

Verified on 2026-10-01 against the official package `26.928.3736.0`:

| Architecture | Desktop version | Bundled CLI | Range body bytes | Requests |
| --- | --- | --- | --- | --- |
| x64 | `26.928.31416` | `0.159.2` | 5,297,477 (about 5.1 MiB) | 22 |
| ARM64 | `26.928.31416` | `0.159.2` | 5,588,152 (about 5.3 MiB) | 23 |

These results include the full resolution of the app version and CLI version. When the small mapping manifest is valid, the MSIX does not need to be read; when the verified installer is unchanged, the cache is reused directly and the installer is not read again. The SHA-256 check covers block hashes taken from the official HTTPS block map; it is not equivalent to downloading the full package and verifying the MSIX signature.

After migrating to upstream main, the versioned address 404 fallback was also verified for x64: full pairing resolution through the official latest package still reads 5,297,477 bytes in 23 requests in total; a second sync needs only 2 requests (the 404 probe of the versioned address and a 1-byte Range probe of the latest package), with 1 byte of Range body in total.

## Selection and manual configuration

- Automatic Desktop / VSCode selects the full pairing by client type, operating system and architecture, preferring the latest app build.
- The minimum CLI version of `auto` compatibility mode only filters candidate pairings and is compared by full semantic version; `0.158.0-alpha.1` does not satisfy `0.158.0`. When no known pairing meets the condition, a non-retryable HTTP 503 is returned before outbound, with error code `codex_client_version_unavailable`, and preview returns 400.
- A manually entered `client_version` or `app_version` overrides the corresponding default respectively, and no pairing is recomputed from the manual version; a manually entered CLI is not raised to the minimum version. When only the app version is entered, the automatic CLI is still filtered by the minimum version.
- The original `user_agent` is kept as is. A manual override is labeled `custom` in preview.
- HTTP, compact, WebSocket, alpha search, live and telemetry share identity resolution. WebSocket pools are isolated by the final UA, Version and Originator; after a configuration or pairing change, the handshake is redone, and a chained connection from an old identity is no longer reused by the new identity. When the version changes, the `previous_response_id` context on an old connection may need to be re-established by the client.

## Cache and management endpoints

The database automatically creates `codex_client_version_cache`, keyed by `(client_kind, target_platform)`. Each target stores only the full version pairing from its last successful sync, without accumulating history. A successful sync updates a single target, and after the transaction succeeds an immutable in-memory snapshot is published; when the sync or the database write fails, the original data and the active version are kept. A restart loads the cache first, even when background sync is turned off. Loading the cache does not modify the database; existing historical pairings are cleaned up manually by the instance administrator.

The read-only `codex_client_versions` in `GET /api/admin/settings` shows the 10 Desktop / VSCode targets with their current pairing, source, status and last successful check time; errors from this sync are returned only in the sync response. Saving ordinary settings does not write this cache back. The compatibility fields `codex_synced_desktop_mac_build`, `codex_synced_desktop_windows_build` and `codex_synced_vscode_build` project macOS ARM64, Windows x64 and VSCode Linux x64 respectively; the old standalone strings are not migrated into pairings.

`POST /api/admin/settings/codex-client-versions/sync` keeps the four result groups `cli`, `desktop_mac`, `desktop_windows` and `vscode`, and adds the CLI version, source, status and per-target results. Concurrent calls are merged, and installer parsing runs with at most 2 concurrent requests; a failure from one source does not block the other sources.

"Sync now" under "System Settings → Codex" in the admin panel syncs the pairings for each architecture and shows their sources. Leaving the CLI/app version input boxes empty uses the automatic pairing, and the default values come from the backend preview. The existing sync switch, interval and `CODEX_DISABLE_CLI_VERSION_SYNC` still apply; this environment variable turns off background sync but not manual sync.

## Verification

Key regression coverage includes ZIP64, Range rejection of the whole package and the budget, version/architecture mismatch, native VSIX versus WSL distinction, independent MSIX blocks and hashes, sparse ASAR, alpha string consistency, cache failure without database writes, restart recovery and concurrent replacement, read-only settings, manual override, unavailable minimum version, and interception before outbound for HTTP / WS.

```sh
go test -tags=http2legacy ./proxy ./proxy/wsrelay ./admin ./database
go test -race -tags=http2legacy ./proxy -run 'TestCodex(ClientVersionCache|ClientVersionSync|ClientCandidate|MSIX|PEVersion|OfficialMSIX|Archive|ClientIdentity)'
cd frontend
pnpm typecheck
pnpm test
pnpm build
```

This run used Go 1.27.1. The existing uTLS / `golang.org/x/net/http2` path triggered nil pointer dereferences in some network tests in this environment, so full verification of the related packages uses the repository's existing `http2legacy` build tag. Reading versions does not depend on that tag itself; version test cases with no network dependency can also be run directly.

References: [Microsoft's MSIX update and 64 KiB block map](https://learn.microsoft.com/en-us/windows/msix/app-package-updates), [discussion of MSIX DEFLATE blocks](https://stackoverflow.com/questions/79511882/are-the-blocks-in-appxblockmap-xml-directly-tied-to-deflate-compressed-blocks-in), [Codex doctor build version marker](https://github.com/openai/codex/blob/main/codex-rs/cli/src/doctor/updates.rs).
