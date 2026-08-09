// Package firmwareimage fetches and inspects the synthetic firmware images
// the simulator flashes, shared by the CWMP Download path and the USP
// Device.DeviceInfo.FirmwareImage.{i}.Download() path so both protocols
// accept the same images.
//
// An "image" is any HTTP-fetchable file that declares its own version on a
// "cpe-labs-firmware-version: <version>" line within the first 64 KiB. The
// simulator carries no firmware version table: the image declares its
// version, so any vendor's versioning scheme works without code changes.
package firmwareimage

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // TR-181 CheckSumAlgorithm includes SHA-1; integrity check, not authentication
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	neturl "net/url"
	"path"
	"strings"
	"time"
)

// VersionHeader is the line prefix a fetched image is scanned for. A test
// author makes a "firmware image" by putting "cpe-labs-firmware-version: X"
// on any line within the first versionScanLimit bytes, followed by arbitrary
// padding.
const VersionHeader = "cpe-labs-firmware-version:"

// versionScanLimit bounds how much of the image body is held in memory for
// the header scan. The rest is drained (and hashed, when a checksum was
// requested) so the serving side observes a complete download; a fleet-scale
// test of the delivery path needs the full transfer to happen.
const versionScanLimit = 64 * 1024

// fetchTimeout bounds the whole image GET, connect through body drain.
// Generous because fleet-scale tests may saturate the image server; a hung
// fetch must still settle as a fault eventually.
const fetchTimeout = 60 * time.Second

// Image is what one fetch observed.
type Image struct {
	// Version is the declared version, empty when no header line was found.
	// The fetch itself succeeded in that case; the caller decides whether a
	// versionless image is a validation failure.
	Version string
	// Digest is the lowercase hex digest of the entire response body under
	// the algorithm passed to Fetch, empty when none was requested.
	Digest string
}

// newHasher maps a TR-181 CheckSumAlgorithm enumeration value onto a hash
// constructor. Returns nil for names outside the enumeration.
func newHasher(alg string) hash.Hash {
	switch alg {
	case "SHA-1":
		return sha1.New() //nolint:gosec // see the import note
	case "SHA-224":
		return sha256.New224()
	case "SHA-256":
		return sha256.New()
	case "SHA-384":
		return sha512.New384()
	case "SHA-512":
		return sha512.New()
	}
	return nil
}

// SupportedChecksumAlgorithm reports whether alg names a hash Fetch can
// compute. The names are the TR-181 CheckSumAlgorithm enumeration.
func SupportedChecksumAlgorithm(alg string) bool {
	return newHasher(alg) != nil
}

// Fetch GETs url the way the observed real device does (one plain GET, no
// range requests), scans the first versionScanLimit bytes for the version
// header, and drains the remainder so the server sees the full download
// complete. When checksumAlg is non-empty the entire body is also hashed, so
// a caller holding a declared checksum can verify the image it received.
//
// A returned error is a transport-level failure (connect, non-200 status, a
// read that died mid-body). A missing version header is NOT an error: the
// transfer worked, the image content is the problem, and the two failure
// classes map to different statuses on the USP side (DownloadFailed vs
// ValidationFailed).
func Fetch(url, checksumAlg string) (Image, error) {
	var hasher hash.Hash
	if checksumAlg != "" {
		hasher = newHasher(checksumAlg)
		if hasher == nil {
			return Image{}, fmt.Errorf("unsupported checksum algorithm %q", checksumAlg)
		}
	}

	client := &http.Client{Timeout: fetchTimeout}
	resp, err := client.Get(url) //nolint:gosec // controller-supplied URL; fetching it is the point
	if err != nil {
		return Image{}, fmt.Errorf("fetch firmware image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Image{}, fmt.Errorf("fetch firmware image: status %d", resp.StatusCode)
	}

	var body io.Reader = resp.Body
	if hasher != nil {
		body = io.TeeReader(resp.Body, hasher)
	}

	prefix := make([]byte, versionScanLimit)
	n, rerr := io.ReadFull(body, prefix)
	if rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return Image{}, fmt.Errorf("read firmware image: %w", rerr)
	}
	// Drain whatever follows the scanned prefix, through the tee so the
	// digest covers the whole image, exactly like a real device that flashes
	// (and verifies) everything it downloaded.
	if _, derr := io.Copy(io.Discard, body); derr != nil {
		return Image{}, fmt.Errorf("read firmware image: %w", derr)
	}

	img := Image{}
	if v, ok := ParseVersionHeader(prefix[:n]); ok {
		img.Version = v
	}
	if hasher != nil {
		img.Digest = hex.EncodeToString(hasher.Sum(nil))
	}
	return img, nil
}

// ParseVersionHeader scans prefix line by line for the version header. Lines
// are whitespace-trimmed; the first match wins. Returns ("", false) when no
// line matches or the matched line carries an empty version.
func ParseVersionHeader(prefix []byte) (string, bool) {
	for _, line := range bytes.Split(prefix, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		if !strings.HasPrefix(s, VersionHeader) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(s, VersionHeader))
		if v == "" {
			return "", false
		}
		return v, true
	}
	return "", false
}

// VersionFromURL derives the version for no-fetch mode: the URL's last path
// segment, stripped of its extension. A purely numeric "extension" is kept,
// it is the tail of a dotted version ("2.0.0"), not a file suffix (".bin").
// Returns "" when the URL does not parse or has no usable last segment; the
// caller treats that as an invalid image.
func VersionFromURL(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return ""
	}
	base := path.Base(u.Path)
	if base == "." || base == "/" {
		return ""
	}
	if ext := path.Ext(base); ext != "" && !isNumericExt(ext) {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// isNumericExt reports whether ext (leading dot included) is digits only,
// which marks it as part of a dotted version rather than a file extension.
func isNumericExt(ext string) bool {
	if len(ext) < 2 {
		return false
	}
	for _, r := range ext[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
