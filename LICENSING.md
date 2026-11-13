# Rigscope licensing

Copyright (c) 2026 Triet (@Triet1402).

Rigscope 1.0.0 and subsequent project-owned code and documentation in this
source tree are licensed under GNU AGPL version 3 only (`AGPL-3.0-only`).
The complete terms are in [LICENSE](LICENSE). This includes the Go hub,
collector, CLI, browser UI, website, scripts and project-owned fixtures.
Third-party software, fonts and license texts retain their respective
licenses; see both THIRD_PARTY_NOTICES files and the font license.

Rigscope 0.2.6 and earlier remain MIT-licensed.

## Corresponding source

Every release provides `rigscope_VERSION_source.tar.gz` beside its binaries.
It contains the exact committed project sources, build and installation scripts,
locked dependency manifests, vendored Go dependencies, and the npm package
sources used in the embedded browser bundle. Source dependencies keep their
own notices. The build toolchain is described in `docs/development.md`.

The hub's Source code link points to the archive for its compiled release.
Development builds point to the repository. If you modify and operate Rigscope
for network users, offer those users the corresponding source for **your
version**, including your changes and build scripts, through a prominent link
at no charge as required by AGPL section 13. Set
`NEXT_PUBLIC_RIGSCOPE_SOURCE_URL` when building a modified UI to provide that
source location. Do not leave it pointing to unmodified upstream source.

The AGPL permits commercial use and paid hosting. License obligations apply
to redistribution and modified network deployments; see the actual license
for the conditions.
