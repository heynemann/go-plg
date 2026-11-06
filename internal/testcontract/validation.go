package testcontract

// Check is the validation input of the test plugins (plg.WithValidation): the validating test plugin's Validate
// does what Mode says, with URL for the requests it makes.
type Check struct {
	Mode string
	URL  string
}
