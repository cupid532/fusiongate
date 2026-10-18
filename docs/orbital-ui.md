# Orbital UI / V3.54

## Design direction

Translate the Geelinx reference into a productive control plane rather than a marketing landing page. Retain administrative workflows, API contracts, data density, explicit destructive actions, and the existing version metadata pipeline.

- Deep-space background, ice-blue actions, restrained electric-blue ambient light.
- Original inline SVG orbital sculpture: no external artwork, fonts, trackers, Canvas, or WebGL dependencies.
- Poster-style login and a compact overview hero, not scroll hijacking or mandatory animation.
- Quiet data panels and consistent pill-shaped actions across the console.
- Dark by default for new users; existing explicit light/dark preferences remain respected.

## Source map

- `web/src/index.css`: semantic light/dark tokens and Tailwind mapping.
- `web/src/cosmic.css`: orbital layout, responsive composition, decoration and reduced-motion rules.
- `web/src/components/GatewayArt.tsx`: original decorative SVG with instance-scoped gradient IDs.
- `web/src/pages/Login.tsx`: editorial entry and accessible administrator form.
- `web/src/pages/Dashboard.tsx`: overview poster, metrics and operational sections.
- `web/src/components/layout/`: navigation and toolbar.
- `web/src/components/ui/`: shared card and action styles.

## Accessibility and behavior

- Sculpture is decorative and does not imply live operational health.
- Status text no longer labels an authenticated session as backend uptime.
- Dashboard metric links support Enter; setup steps are real anchors; setup disclosure exposes expanded state.
- Skip-to-content focuses the main region without changing application routing.
- Login preserves password-manager semantics and announces associated failures.
- CSS respects reduced motion; MotionConfig reduces console motion. Desktop hover is not needed to access controls.
- Mobile composition changes rather than shrinking the desktop poster.

## Validation

- `deploy/build-web.sh` stages generated assets for Go embedding.
- `npx tsc -b`, `npm run build`, `npx oxlint`, `npx vitest run`.
- Frontend: 34 test files / 176 passing tests at implementation time. Oxlint completes with 23 warnings in existing patterns.
- `go test ./...`, `go vet ./...`, `git diff --check` pass.
- Playwright smoke checks with intercepted fixture API responses: login and dashboard at 1440px and 390px, light theme toggle, mobile drawer and Escape. No page errors or document-level horizontal overflow in these checks.
- Fixture browser checks do not prove live provider availability or all administrative mutation workflows.

## Delivery boundary

These changes are prepared in the source checkout. Production release still requires committing and pushing the source and generated embedded assets, then using the repository's deployment workflow. Do not edit deployed release trees or bypass deployment parity checks.
