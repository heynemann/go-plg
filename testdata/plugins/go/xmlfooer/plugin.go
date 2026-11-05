// Package xmlfooer imports packages outside the default Go guest's allowlist: encoding/xml from the standard
// library and example.com/greet, a third-party module, with its packages whose paths end in _test and _linux.
// Only a custom guest built with all of them can load it.
package xmlfooer

import (
	"context"
	"encoding/xml"

	"example.com/greet"
	"example.com/greet/helper_linux"
	"example.com/greet/helper_test"

	"github.com/heynemann/go-plg/internal/testcontract"
)

type person struct {
	XMLName xml.Name `xml:"person"`
	Name    string   `xml:"name,attr"`
}

type fooer struct{}

// Create returns a plugin instance.
func Create() (testcontract.Fooer, error) {
	return fooer{}, nil
}

// Foo greets name, encoded as XML, and decodes the XML again to prove the round trip.
func (fooer) Foo(_ context.Context, name string) (string, error) {
	doc, err := xml.Marshal(person{Name: name})
	if err != nil {
		return "", err
	}

	var back person

	err = xml.Unmarshal(doc, &back)
	if err != nil {
		return "", err
	}

	return greet.Hello(string(doc)) + " / " + back.Name + " / " + helpertest.Suffix() + "," + helperlinux.Suffix(), nil
}

func (fooer) Count(context.Context) (int, error) { return 0, nil }

func (fooer) Fail(context.Context, string) error { return nil }

func (fooer) Boil(context.Context, testcontract.Celsius) (bool, float64, error) { return false, 0, nil }

func (fooer) Scalars(
	_ context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
	u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string,
) (bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, string, error) {
	return b, i, i8, i16, i32, i64, u, u8, u16, u32, u64, f32, f64, s, nil
}

func (fooer) Spin(context.Context, int) (int, error) { return 0, nil }
