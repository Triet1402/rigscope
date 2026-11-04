# Packaging reference files

The `.env.example` files contain public examples, never deployment credentials. Copy configuration to private files (0600) outside the source tree and generate fresh credentials. No configuration starts a production deployment by itself.

Use [installation](../docs/installation.md) for managed user/system installs, [operations](../docs/operations.md) for manifest archives and recovery, and [releasing](../docs/releasing.md) for package validation. System reference units are in [system](system/README.md). Generated units in the installer source are authoritative.
