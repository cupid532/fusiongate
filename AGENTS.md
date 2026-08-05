# FusionGate Agent Instructions

These instructions apply to every automated agent and contributor working in this repository.

## Versioning

- The source of truth for the product version is `internal/fusiongate/version.go`.
- Every product update must increment `fusiongate.Version` in the same change. Do not merge or push an update without a version bump.
- If the requested update level is not specified, treat it as a normal update.
- Normal update: add `0.01` (for example, `V1.0` -> `V1.01` -> `V1.02`).
- Major update: add `0.1` (for example, `V1.0` -> `V1.1`).
- Extraordinary update: add `1` (for example, `V1.0` -> `V2.0`).
- Calculate versions as integer hundredths, not floating-point numbers. Format with at least one decimal digit and omit only insignificant trailing hundredths (`V1.10` is displayed as `V1.1`, while `V1.01` stays `V1.01`).
- A version-only correction does not require another version bump.
- Keep the sidebar version display wired to `fusiongate.Version`; do not hard-code a second version in HTML.
- Update or add tests whenever version rendering or release behavior changes.

## Verification

- Run `gofmt` on changed Go files.
- Run `go test ./...` and `go vet ./...` before pushing.
- Never commit `.env` files, databases, credentials, secrets, or deployment data.
