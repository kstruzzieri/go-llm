### Fixed — AgentFlow children get PWD for the directory they run in (#624)

AgentFlow and its gates receive `PWD` set to the directory they run in. Go
derives `PWD` this way only when a child inherits its parent's environment, so
the from-scratch environment of #577 sets it explicitly. In v0.4.0, source mode
and `golem audit` passed Golem's own `PWD`, which was wrong whenever Golem ran
from another directory. Programs that read `PWD` directly, such as a Makefile
using `$(PWD)`, see the right directory. `PWD` keeps the directory's spelling,
so a logical path such as `/tmp/x` can differ textually from the physical
`/private/tmp/x`; Windows children do not get one.

`PWD` is runner-owned: `-agentflow-env PWD` and
`(*agentflow.ExecRunner).AllowEnv("PWD")` are rejected, like `PYTHONPATH`.
