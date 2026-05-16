package file

import (
	"net/url"

	"github.com/s1na/geth-ferry/pkg/backend"
)

func init() {
	backend.Register("file", func(u *url.URL, _ *backend.OpenConfig) (backend.Backend, string, error) {
		be, err := FromURL(u)
		if err != nil {
			return nil, "", err
		}
		// file:// backend is rooted at the URL path; in-backend prefix is empty.
		return be, "", nil
	})
}
