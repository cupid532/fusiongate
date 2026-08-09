# FusionGate Agent Instructions

These instructions apply to every automated agent and contributor working in this repository.

## Versioning

- The source of truth for the product version is `internal/fusiongate/version.go`.
- Every product update must increment `fusiongate.Version` in the same change. Do not merge or push an update without a version bump.
- If the requested update level is not specified, treat it as a normal update.
- Normal update: add `0.01` (for example, `V1.00` -> `V1.01` -> `V1.02`).
- Major update: add `0.1` (for example, `V1.29` -> `V1.30`).
- Extraordinary update: add `1` (for example, `V1.30` -> `V2.30`).
- Calculate versions as integer hundredths, not floating-point numbers.
- **Always render exactly two decimal digits.** `V1.30`, never `V1.3`; `V1.10`, never `V1.1`; `V1.00`, never `V1.0`. Trailing zeros are significant here: dropping them made a bump from `V1.29` to `V1.30` display as `V1.3`, which reads like a downgrade to anyone looking at the sidebar.
- A version-only correction does not require another version bump.
- Keep the sidebar version display wired to `fusiongate.Version`; do not hard-code a second version in HTML.
- `TestVersionUsesTwoDecimalDigits` enforces the format. Update or add tests whenever version rendering or release behavior changes.

## Verification

- Run `gofmt` on changed Go files.
- Run `go test ./...` and `go vet ./...` before pushing.
- Never commit `.env` files, databases, credentials, secrets, or deployment data.
