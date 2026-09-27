### Added — Child scope-denial reporting (#555)

Opt-in interceptors now report actual native workspace refusals from scoped
dispatch children in the parent's risk report. Each affected child contributes
10 points per refused request, capped at 100 per dispatch invocation. Reporting
uses native evidence independently of child summaries and scores; legacy
unscoped tasks remain excluded. Findings are informational and do not change
filesystem enforcement or grants.
