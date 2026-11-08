// A separate module on purpose: embedded-postgres is a local convenience for
// running the Postgres tests without Docker, and must not become a dependency
// of Claviger itself (CI uses a postgres service container instead).
module claviger-pgembedded

go 1.27

require github.com/fergusstrange/embedded-postgres v1.34.0

require (
	github.com/lib/pq v1.10.9 // indirect
	github.com/xi2/xz v0.0.0-20171230120015-48954b6210f8 // indirect
)
