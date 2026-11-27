### Fixed — llm-bench injection registration refuses a repeated model (#876)

`llm-bench` now refuses an injection registration that lists the same model twice. Before, a repeated model also let a decoding entry for a model that was not listed load without an error.
