package domain

import "errors"

var (
	ErrNotFound = errors.New("not found")
	ErrNoTenant = errors.New("no tenant in context")
	ErrConflict = errors.New("conflict")
)

var (
	ErrToolNotFound = errors.New("tool not found")
	ErrToolSchema   = errors.New("tool schema violation")
)
