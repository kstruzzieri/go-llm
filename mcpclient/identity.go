package mcpclient

// connectionIdentity is a server's frozen launch or destination identity
// (spec §5.6). Values can be secret: argv and endpoint are never rendered and
// no value is persisted; only keyed digests leave the process.
type connectionIdentity struct {
	workspace, alias, kind string
	// stdio
	envBaseline           string
	env                   []string // source:NAME, sorted
	launcher, target, dir string
	argv                  []string
	// http
	origin   string // canonical scheme://host[:port]
	endpoint string // canonical path plus optional query
}
