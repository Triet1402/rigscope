package protocol

// Version is the wire protocol version carried in the optional hello.protocol
// and welcome.protocol fields. A hub accepts the current and the previous
// version; an older or newer version is rejected.
const Version = 1

// Wire size and cardinality limits. They mirror src/contracts/telemetry.ts.
const (
	// MaxMessageBytes is the largest message of any type except detail results.
	MaxMessageBytes = 262_144
	// MaxDetailBytes is the largest detail.result message (process tables).
	MaxDetailBytes = 1_048_576
	// MaxBatchRecords is the largest number of records in one samples message.
	MaxBatchRecords = 32
	// MaxInFlightBatches is the largest unacknowledged batch window a hub may advertise.
	MaxInFlightBatches = 64
	// MaxPersistentFlushMS is the shared hub flush bound at the fastest 100 ms cadence.
	MaxPersistentFlushMS = 5_000

	MaxCores                 = 1024
	MaxGPUs                  = 16
	MaxFilesystems           = 64
	MaxDisks                 = 64
	MaxInterfaces            = 64
	MaxAddressesPerInterface = 32
	MaxProcessRows           = 6_000
	MaxCollectionFailures    = 32

	MinDetailIntervalMS = 1_000
	MaxDetailIntervalMS = 60_000
	MinDetailLeaseMS    = 5_000
	MaxDetailLeaseMS    = 120_000

	// MinExpectedIntervalMS and MaxExpectedIntervalMS bound the declared
	// delivery cadence advertised in hello.expectedIntervalMs.
	MinExpectedIntervalMS = 100
	MaxExpectedIntervalMS = 7_200_000
)

// Field-level limits that are private in the TypeScript contract but are part
// of the same wire rules.
const (
	MaxIDLength         = 128
	MaxResourceIDLength = 256
	MaxTextLength       = 256
	MaxHostnameLength   = 255
	MaxTimestampLength  = 64

	// MaxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER, the largest
	// integer the protocol may carry.
	MaxSafeInteger int64 = 9_007_199_254_740_991

	// MaxAckRecords is the largest accepted or rejected list in one ack.
	MaxAckRecords = MaxBatchRecords * 64
)

// Timestamp sanity window: values outside it are typos or clock corruption
// rather than telemetry.
const (
	minTimestampYear = 2000
	maxTimestampYear = 2200
)
