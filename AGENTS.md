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
- For console changes, also run `npm run build`, `npx tsc -b`, `npx oxlint`, and `npx vitest run` in `web/`.
- `go.mod` requires Go 1.25; if the host toolchain is older, run the Go steps in a container:
  `docker run --rm -v "$PWD:/src" -w /src golang:1.25.12-alpine sh -c "apk add --no-cache build-base && go test ./..."`
- Never commit `.env` files, databases, credentials, secrets, or deployment data.

## Deployment parity

The deployed host and `origin/main` must always agree on version and behaviour. Practically:

- **Push before you deploy.** `deploy/deploy-from-origin.sh` refuses any commit that is not an ancestor of `origin/main`, refuses a dirty working tree, and refuses a commit whose `Version` matches what is already running. Do not work around these checks; fix the cause.
- **Never edit the deployed tree.** `/opt/fusiongate/app` and `/opt/fusiongate/releases/*` are build artifacts. Source changes belong in the git checkout, committed and pushed. A previous flow lost this and left the host running code that existed in no commit.
- **The running process must be identifiable.** `/healthz` reports `version` and `revision`; `revision` must equal a real commit SHA on `origin/main`. Keep it that way when touching build flags or the Dockerfile.
- Built console assets under `internal/fusiongate/ui/` are committed so native `go build`/`go test` can satisfy `//go:embed ui`. The Docker build regenerates them from `web/`, so `web/` is the source of truth — run `deploy/build-web.sh` and commit the result alongside any console change.
