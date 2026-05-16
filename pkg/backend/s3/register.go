package s3

import (
	"context"
	"net/url"

	"github.com/s1na/geth-ferry/pkg/backend"
)

func init() {
	backend.Register("s3", func(u *url.URL, cfg *backend.OpenConfig) (backend.Backend, string, error) {
		return FromURL(context.Background(), u, cfg)
	})
}
