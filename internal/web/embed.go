// Package web holds the dashboard's static assets.
package web

import "embed"

// Assets are embedded rather than fetched from a CDN because the dashboard has
// to work when the internet is down - which is precisely when someone opens it.
//
//go:embed assets
var Assets embed.FS
