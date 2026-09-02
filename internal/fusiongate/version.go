package fusiongate

// Version is the user-visible FusionGate release version.
//
// It always carries exactly two decimal digits. Trailing zeros are significant:
// abbreviating V1.30 to V1.3 makes a bump from V1.29 look like a downgrade in the
// console sidebar. See AGENTS.md for the increment rules.
const Version = "V2.72"

// BuildRevision is injected by release builds so a running process can be
// matched to the exact source commit, not just the user-visible version.
var BuildRevision = "unknown"
