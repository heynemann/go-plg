//go:build !integration

package plg_test

// prepareGuests prepares nothing for the unit tests, which load no guest; the integration build's compiles the
// guests into the compilation cache (customguest_integration_test.go).
func prepareGuests() error {
	return nil
}
