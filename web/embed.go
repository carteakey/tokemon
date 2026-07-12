package web

import "embed"

// StaticFS contains the dashboard's local, original visual assets.
//
//go:embed static/**
var StaticFS embed.FS
