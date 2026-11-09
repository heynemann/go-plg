// Package everything is a contract whose interface, Host and validation input use every data type plg carries,
// for the typings' golden files.
package everything

import (
	"context"
	"time"
)

// Store is the interface plugins implement.
type Store interface {
	Scalars(ctx context.Context, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64, u uint, u8 uint8,
		u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, s string) (bool, error)
	Bytes(ctx context.Context, raw []byte, blob Blob) ([]byte, error)
	Times(ctx context.Context, at time.Time, when *time.Time, every time.Duration) (Stamped, error)
	Collections(ctx context.Context, tags []string, matrix [][]int, byName map[string]*Item,
		nested map[string][]Item) (Tags, error)
	Records(ctx context.Context, item Item, ptr *Item, point struct{ X, Y int }, empty struct{}) (*Item, Tree, error)
	Nothing(ctx context.Context) error
	Variadic(ctx context.Context, first string, rest ...int) (int, error)
	Unnamed(context.Context, string, int) (Tier, error)
	Keywords(ctx context.Context, function string, end int, _ bool, p3 number) error
	Embedding(ctx context.Context, e Embedder) (Count, error)
}

// Directory is the Host.
type Directory interface {
	Lookup(ctx context.Context, id string, at time.Time) (*Item, bool, error)
	Notify(ctx context.Context, items ...Item) error
	Ping(ctx context.Context) error
}

// Limits is the validation input.
type Limits struct {
	MaxItems int       `json:"maxItems"`
	Until    time.Time `json:"until"`
}

// Item is a record with tagged, skipped, renamed and recursive fields.
type Item struct {
	Seen     time.Time          `json:"seen"`
	Attrs    map[string]float64 `json:"attrs"`
	Parent   *Item              `json:"parent"`
	Name     string             `json:"name,omitempty"`
	Secret   string             `json:"-"`
	Dash     string             `json:"-,"`
	Kebab    string             `json:"a-b"`
	Accent   string             `json:"café"`
	End      string             `json:"end"`
	Children []*Item            `json:"children"`
	Embedded
	ID int64 `json:"id"`
}

// Embedded is embedded in Item.
type Embedded struct {
	Note string
}

// Embedder embeds a struct and an alias of it.
type Embedder struct {
	Counter
	Alias
}

// Counter counts.
type Counter struct {
	N int
}

// Alias is another name for Counter.
type Alias = Counter

// Stamped holds times, absent and present.
type Stamped struct {
	Prev    *time.Time
	History []time.Time
	At      time.Time
}

type (
	// Blob is named bytes.
	Blob []byte
	// Tier is a named string.
	Tier string
	// Tags is a named slice.
	Tags []string
	// Count is a named integer.
	Count int
	// Tree is a recursive slice.
	Tree []Tree
	// number takes a name TypeScript reserves.
	number int
)
