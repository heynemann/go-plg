module example.com/plugins/goxml

go 1.27

require (
	example.com/directives v1.0.0
	github.com/heynemann/go-plg v0.0.0
)

// The custom guest is built with this checkout's plg (a plg command built from a checkout has no version).
replace github.com/heynemann/go-plg => ../../../..
