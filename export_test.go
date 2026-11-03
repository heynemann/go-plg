package plg

import "context"

// Hold takes the instance of the plugin with the given key, as a call in flight would, until the returned
// release function is called. It lets tests make a plugin busy without a slow plugin method.
func Hold(ctx context.Context, c *Container, key string) (func(), error) {
	plug, ok := c.catalogue.Load().plugins[key]
	if !ok {
		return nil, ErrNotFound
	}

	held, err := plug.pool.acquire(ctx)
	if err != nil {
		return nil, err
	}

	return func() { plug.pool.release(held) }, nil
}
