/*
Copyright 2026 The Kubermatic Kubernetes Platform contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maintenance

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/client-go/rest"
)

const (
	// maxAssetSize limits every embedded asset. The page is stored in a ConfigMap (max. 1 MiB)
	// and up to three assets (logo, dark logo, favicon) are embedded base64-encoded.
	maxAssetSize = 200 * 1024

	assetFetchTimeout = 10 * time.Second
)

// allowedAssetTypes are the content types that are embedded.
var allowedAssetTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/svg+xml":            true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
}

// isRelativeURL reports whether the dashboard resolves the URL against its own
// origin, e.g. "/branding/logo.svg" or "assets/branding/logo.svg".
func isRelativeURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "data:") {
		return false
	}

	u, err := url.Parse(raw)

	return err == nil && u.Scheme == "" && u.Host == ""
}

// AssetFetcher loads a branding asset from the dashboard. path is the URL as
// configured in the branding (e.g. "/branding/logo.svg"). The returned content
// type may be empty if it is not known, it is then derived from the content.
type AssetFetcher func(ctx context.Context, path string) (body []byte, contentType string, err error)

// ResolveAssets replaces relative branding URLs (files mounted into the
// dashboard container) by data URIs. Relative URLs cannot be served by the
// maintenance page itself, because they would be answered with the maintenance
// response, too. The assets are therefore fetched from the dashboard before the
// maintenance mode is switched on.
//
// Assets that cannot be fetched are dropped and reported via warn, the page then
// falls back to a text-only header.
func ResolveAssets(ctx context.Context, fetch AssetFetcher, b Branding, warn func(format string, args ...any)) Branding {
	for _, field := range []*string{&b.LogoURL, &b.LogoDarkURL, &b.FaviconURL} {
		if !isRelativeURL(*field) {
			continue
		}

		dataURI, err := fetchAsDataURI(ctx, fetch, *field)
		if err != nil {
			if warn != nil {
				warn("could not embed branding asset %q, it will not be shown on the maintenance page: %v", *field, err)
			}
			*field = ""

			continue
		}

		*field = dataURI
	}

	return b
}

// HTTPAssetFetcher fetches assets from baseURL using plain HTTP(S).
func HTTPAssetFetcher(client *http.Client, baseURL string) AssetFetcher {
	return func(ctx context.Context, path string) ([]byte, string, error) {
		base, err := url.Parse(baseURL)
		if err != nil {
			return nil, "", fmt.Errorf("invalid base URL: %w", err)
		}

		ref, err := url.Parse(strings.TrimSpace(path))
		if err != nil {
			return nil, "", fmt.Errorf("invalid URL: %w", err)
		}
		ref.Path = "/" + strings.TrimPrefix(ref.Path, "/")
		target := base.ResolveReference(ref)

		ctx, cancel := context.WithTimeout(ctx, assetFetchTimeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, "", err
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, "", err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("%s returned HTTP %d", target, resp.StatusCode)
		}

		body, err := readLimited(resp.Body)

		return body, resp.Header.Get("Content-Type"), err
	}
}

// ServiceProxyAssetFetcher fetches assets from a Service in the cluster via the
// Kubernetes API server proxy. This only needs the credentials the installer
// already has: no DNS, load balancer or TLS trust towards the public domain.
func ServiceProxyAssetFetcher(client rest.Interface, namespace, service string, port int) AssetFetcher {
	return func(ctx context.Context, path string) ([]byte, string, error) {
		ctx, cancel := context.WithTimeout(ctx, assetFetchTimeout)
		defer cancel()

		ref, err := url.Parse(strings.TrimSpace(path))
		if err != nil {
			return nil, "", fmt.Errorf("invalid URL: %w", err)
		}

		stream, err := client.Get().
			Namespace(namespace).
			Resource("services").
			Name(fmt.Sprintf("%s:%d", service, port)).
			SubResource("proxy").
			Suffix(strings.TrimPrefix(ref.Path, "/")).
			Stream(ctx)
		if err != nil {
			return nil, "", err
		}
		defer stream.Close()

		body, err := readLimited(stream)

		// the proxy does not hand out the content type
		return body, "", err
	}
}

func readLimited(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxAssetSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxAssetSize {
		return nil, fmt.Errorf("asset is larger than %d KiB", maxAssetSize/1024)
	}

	return body, nil
}

func fetchAsDataURI(ctx context.Context, fetch AssetFetcher, path string) (string, error) {
	body, rawType, err := fetch(ctx, path)
	if err != nil {
		return "", err
	}

	contentType, _, err := mime.ParseMediaType(rawType)

	if err != nil || contentType == "application/octet-stream" || contentType == "text/plain" {
		// some servers do not know SVG files, and the API server proxy does not tell the type at all
		ref, _ := url.Parse(strings.TrimSpace(path))
		if ref != nil && strings.HasSuffix(strings.ToLower(ref.Path), ".svg") {
			contentType = "image/svg+xml"
		} else {
			contentType = http.DetectContentType(body)
		}
	}

	if !allowedAssetTypes[contentType] {
		return "", fmt.Errorf("unsupported content type %q", contentType)
	}

	return fmt.Sprintf("data:%s;base64,%s", contentType, base64.StdEncoding.EncodeToString(body)), nil
}
