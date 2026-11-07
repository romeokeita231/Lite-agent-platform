package domain

import "errors"

var (
	ErrNotFound = errors.New("not found")
	ErrNoTenant = errors.New("no tenant in context")
	ErrConflict = errors.New("conflict")
)

var (
	ErrToolSchema         = errors.New("tool schema violation")
	ErrModelRouteNotFound = errors.New("model route not found")
)
