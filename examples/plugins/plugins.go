// Package plugins holds the example pricing rules, one directory per plugin in plg's built-in storage layout (a
// plugin.yaml next to the plugin's files), one rule per language:
//
//   - bulk (Go): 10% off orders of at least 10 items;
//   - loyalty (Lua): a percentage off by the customer's tier, which it asks the service for;
//   - coupon (TypeScript): a fixed amount off for a known coupon code.
//
// The service serves them with plg.NewFSStorageFromFS(FS); the storage examples upload the same trees.
package plugins

import "embed"

// FS holds the plugin directories bulk, coupon and loyalty.
//
//go:embed bulk coupon loyalty
var FS embed.FS
