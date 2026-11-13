package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/dialohq/roamvm/internal/daemon"
	"github.com/dialohq/roamvm/internal/state"
)

// Only Get is used by a pager; mutations remain in the local writable cache.
type chunkSource struct {
	state.Store
	client *http.Client
}

func (s chunkSource) Get(ctx context.Context, key string) (state.Object, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://runtime/chunk?key="+url.QueryEscape(key), nil)
	if err != nil {
		return state.Object{}, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return state.Object{}, err
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return state.Object{}, fmt.Errorf("checkpoint chunk fetch: HTTP %d", res.StatusCode)
	}
	return state.Object{Body: res.Body, Size: res.ContentLength}, nil
}

func storagePath(p *daemon.Prepared, name string) string {
	path := filepath.Join(p.Dir, name)
	if p.PagerDir != "" {
		if _, err := os.Stat(path + ".lazy"); err == nil {
			return filepath.Join(p.PagerDir, name)
		}
	}
	return path
}
