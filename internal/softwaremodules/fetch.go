package softwaremodules

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/ispx-limited/cpe-labs/internal/paramtree"
)

// maxManifestBytes bounds one fetched manifest. An app manifest is a
// few kilobytes of YAML; anything larger is not one.
const maxManifestBytes = 4 << 20

// fetchTimeout bounds one fetch. TR-369 gives the device 24 hours to
// finish an install, but a manifest that takes longer than this to
// arrive is a server that is not going to answer.
const fetchTimeout = 30 * time.Second

// Fetcher resolves a deployment unit's URL to its manifest.
type Fetcher func(ctx context.Context, rawURL string) (*paramtree.AppManifest, *Fault)

// Fetch is the default Fetcher: a plain GET, the body read as a
// manifest. A connection or timeout failure is the server being
// unreachable, a non-2xx status is the file being unavailable, and a
// body that does not load as a manifest is corrupt; the three are
// distinct fault codes on both protocols. Username and Password input
// arguments are accepted by the callers and ignored here, the same
// convention the firmware path uses.
func Fetch(ctx context.Context, rawURL string) (*paramtree.AppManifest, *Fault) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fault(ReasonInvalidArguments, "URL %q is not an absolute URL", rawURL)
	}
	if u.User != nil {
		// TR-181: a userinfo component in the URL is rejected outright.
		return nil, fault(ReasonInvalidArguments, "URL carries a userinfo component")
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fault(ReasonInvalidArguments, "%v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fault(ReasonUnreachable, "%v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fault(ReasonUnavailable, "server answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fault(ReasonUnreachable, "reading body: %v", err)
	}
	if len(body) > maxManifestBytes {
		return nil, fault(ReasonCorrupt, "manifest exceeds %d bytes", maxManifestBytes)
	}
	m, lerr := paramtree.LoadAppManifest(bytes.NewReader(body), rawURL)
	if lerr != nil {
		return nil, fault(ReasonCorrupt, "%v", lerr)
	}
	return m, nil
}
