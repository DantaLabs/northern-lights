// Package demoenv holds the shared dependencies handed to every demo lane
// constructor (change set, section recipe, section text, data).
package demoenv

import (
	"database/sql"

	"github.com/dantalabs/northern-lights/internal/audit"
	"github.com/dantalabs/northern-lights/internal/config"
	"github.com/dantalabs/northern-lights/internal/mapping"
	"github.com/dantalabs/northern-lights/internal/workivaprovider"
)

// Env is the set of shared dependencies passed to lane constructors.
type Env struct {
	DB     *sql.DB
	Store  *mapping.Store
	Audit  *audit.Log
	Client workivaprovider.Backend
	Cfg    *config.Config
}
