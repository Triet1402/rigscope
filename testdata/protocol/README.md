# Protocol golden fixtures

One JSON message per file, shared by the Go (`internal/protocol`) and
TypeScript (`tests/protocol-fixtures.test.ts`) test suites.

- `valid/<name>.json` must decode.
- `invalid/<name>.json` must be rejected; the file name states the defect.
- `reencoded/<name>.json` is generated from the corresponding valid fixture by
  the Go decoder + encoder. Do not edit it by hand; run:

  ```
  go generate ./internal/protocol
  ```

  The generator also builds a few `valid/*-constructed*` fixtures directly from
  Go values through the validated outbound encoder (`EncodeCollectorMessage` /
  `EncodeHubMessage`), so the TypeScript corpus covers construction-side output
  as well as decode/encode round trips.

The direction is encoded in the file-name prefix because both suites pick a
parser from it:

- `collector-*` is collector → hub (`DecodeCollectorMessage`,
  `parseCollectorMessage`).
- `hub-*` is hub → collector (`DecodeHubMessage`, `parseHubMessage`).

Fixtures cover every message type, both metric status variants, gap records,
acks with rejected entries, optional `protocol` present/absent/invalid, and the
length/array/range limits. Byte-size limits are enforced on the raw frame by the
Go decoder entry point and are covered by Go unit tests; the oversized fixture in
`invalid/` is a valid object message whose only defect is its size, so the
TypeScript object parsers accept it by design.
