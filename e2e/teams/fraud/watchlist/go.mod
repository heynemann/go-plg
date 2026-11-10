module example.com/fraud/watchlist

go 1.27

require (
	example.com/rewards v1.0.0
	github.com/heynemann/go-plg v0.0.0-00010101000000-000000000000
)

// Only because this team's repository lives in plg's own: an author's module requires a released plg.
replace github.com/heynemann/go-plg => ../../../..
