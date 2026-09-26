### Fixed — Track retrieval registration through collector shutdown (#569)

Background feedback collectors reject new retrieval registrations after shutdown
begins and wait for admitted registrations to finish persistence and window
installation before `Close` returns. Manual collector lifecycle behavior is
unchanged.
