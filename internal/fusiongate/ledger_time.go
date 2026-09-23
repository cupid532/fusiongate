package fusiongate

// Ledger timestamps are written in UTC using RFC3339Nano, whose optional and
// variable-width fraction is not lexically sortable at second boundaries.
// Compare fixed-width fractions without losing nanoseconds to SQLite julianday.
const ledgerCreatedAtSQL = `(substr(l.created_at,1,19)||'.'||substr(
 CASE WHEN substr(l.created_at,20,1)='.' THEN rtrim(substr(l.created_at,21),'Z') ELSE '' END
 ||'000000000',1,9))`

const ledgerTimeLayout = "2006-01-02T15:04:05.000000000"
