package server

import "github.com/kenwea-protocol/agentirc/internal/store"

// The log lives in its own package (internal/store); these names keep the
// server code short.
type (
	Line  = store.Line
	Store = store.Store
)

var (
	NormalizeChannel = store.NormalizeChannel
	ErrBadInput      = store.ErrBadInput
	OpenStore        = store.OpenStore
)

const (
	KeepInRAM = store.KeepInRAM
	MaxText   = store.MaxText
)
