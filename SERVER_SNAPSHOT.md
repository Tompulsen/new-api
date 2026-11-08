# Server source snapshot

This repository imports the server's current New API source on 2026-11-08
(Asia/Shanghai), including its community and studio SSO customizations.
It is a source snapshot; the server source directory had no Git history.

## Provenance

- Upstream: `QuantumNous/new-api`
- Upstream base: `706144769e5bed5a47b78721e3bb11da3cf4c0b9`
- Running image tag: `community-embedded-newapi:20261107-15eea17f0634`
- Running image ID: `sha256:15eea17f063433e1f4f6b918948f5b9a0bcef7282abd4ebd5a6f06ab34e7bfe1`
- Executable SHA-256: `891b1f52152081948c423712b3457913be3e30819ecf51d19c3c6c6040fa5a31`

The executable in the source directory and `/new-api` in the running container
had identical SHA-256 hashes. All 2,683 imported source files were checked against
their server copies. The source files, upstream license, notices, and attribution
are preserved without application-code changes.

## Scope

The snapshot excludes dependency directories, generated frontend distributions,
the compiled server executable, runtime databases and logs, deployment secrets,
and three temporary `*.before-search-fix` backup files.
The upstream `.env.example` and Compose examples are included as source templates;
they are not copies of the server's live configuration.

Use the standard `Dockerfile` and upstream build instructions when rebuilding.
`Dockerfile.sso` is preserved as historical build context and references a
server-local base image and executable that are not included in this repository.

The repository was created as private with `main` as its default branch.
GitHub Actions was disabled before the initial push to prevent imported workflows
from automatically building or publishing. No application deployment was performed.
