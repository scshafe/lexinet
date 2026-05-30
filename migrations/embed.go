package migrations

import "embed"

// FS contains lexinet database migrations for golang-migrate/iofs.
//
//go:embed *.sql
var FS embed.FS
