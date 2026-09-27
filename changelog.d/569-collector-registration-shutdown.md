### Fixed — Track retrieval registration through collector shutdown (#569)

Background feedback collectors reject new retrieval registrations and
explicit-time records (`RegisterRetrieval`, `RegisterRetrievalAt`, `RecordAt`,
`RecordBatchAt`) after shutdown begins, matching `Record`. `Close` waits for
admitted registrations and records to finish persistence, window installation,
and recomputation, then runs its final sweep, so a window an admitted
registration installs during shutdown still receives its expiry signals.
Manual collector lifecycle behavior is unchanged.
