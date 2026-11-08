package testcase

import (
	"errors"
	"fmt"
	"io/fs"
	"reflect"

	"github.com/heynemann/go-plg/internal/offline"
)

// ExamplesGlob matches a plugin's example validation inputs: the inputs plg validate calls the plugin's Validate
// with, as services registered with plg.WithValidation would.
const ExamplesGlob = "examples/validation/*.yaml"

// examplePath names an example's value in errors.
const examplePath = "example"

// ErrNoValidation reports example validation inputs of a plugin whose contract declares no validation input.
var ErrNoValidation = errors.New("the contract declares no validation input (//plg:validation)")

// Example is an example validation input: a file of [ExamplesGlob] whose content is a value of the contract's
// validation input, written as test case values are ([Load]); {$ref: <path>} takes it from another file of the
// plugin, such as a test case's fixture.
type Example struct {
	// Err is why the file cannot be read or decoded.
	Err error
	// Input is the decoded value, of the contract's validation input type.
	Input reflect.Value
	// Name is its path in the plugin's directory.
	Name string
}

// Examples reads the example validation inputs of the plugin whose files are files, in name order, decoding them
// into contract's validation input. Without one, every example fails with [ErrNoValidation].
func Examples(contract *offline.Contract, files fs.FS) []Example {
	names, _ := fs.Glob(files, ExamplesGlob) //nolint:errcheck // the only error is a malformed pattern, and ours is not

	examples := make([]Example, len(names))

	for i, name := range names {
		examples[i] = Example{Name: name}

		if contract.Validation == nil {
			examples[i].Err = fmt.Errorf("%s: %w", name, ErrNoValidation)

			continue
		}

		root, err := readYAML(files, name)
		if err == nil {
			examples[i].Input, err = (&decoder{files: files}).decode(name, root, contract.Validation.Input,
				examplePath)
		}

		examples[i].Err = err
	}

	return examples
}

// ValidateWith is the call of the plugin's Validate with input, a value of contract's validation input.
func ValidateWith(contract *offline.Contract, input reflect.Value) *offline.Call {
	return &offline.Call{Args: []reflect.Value{input}, Method: len(contract.Methods)}
}
