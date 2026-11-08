module example.com/pricing-rules/bundle

go 1.27

require (
	github.com/heynemann/go-plg v0.0.0-00010101000000-000000000000
	github.com/heynemann/go-plg/examples v1.0.0
)

// Only because this example lives in plg's own repository: an author's module requires a released plg.
replace github.com/heynemann/go-plg => ../../..
