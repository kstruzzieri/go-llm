### Added — published containment declarations and coverage inventory (#452)

- `docs/security/containment.md` declares, per scenario, which forbidden effects Golem's shipped boundaries contain when a model follows an attack, which they do not (owned residuals such as #420), and which depend on pending work; `docs/security/containment-inventory.md` maps every #452 requirement to executed, mutation-proven evidence. No new runtime containment is introduced.
- The pre-push and CI security gate now also runs the containment scenarios and verifies each mandatory inventory test executed and passed.
