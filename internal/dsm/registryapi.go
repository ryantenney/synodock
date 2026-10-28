package dsm

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// manifestTypes are the manifest media types a registry may answer with: indexes first, so
// a multi-platform image resolves to its linux/amd64 manifest.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// RegistryImageID is the short config digest of the linux/amd64 image repo:tag in a registry
// (the id the NAS shows once it has that image), or "" if the registry doesn't have it. It
// uses the registry's own v2 API with basic auth.
func RegistryImageID(registryURL, repo, tag, user, password string) (string, error) {
	base, err := url.Parse(registryURL)
	if err != nil {
		return "", err
	}
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	client := &http.Client{Timeout: 30 * time.Second}
	get := func(ref string) (map[string]any, error) {
		req, err := http.NewRequest("GET", strings.TrimSuffix(registryURL, "/")+"/v2/"+repo+"/manifests/"+ref, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", auth)
		req.Header.Set("Accept", manifestTypes)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		if resp.StatusCode != http.StatusOK {
			return nil, Errorf("registry %s: HTTP %d for %s:%s", base.Host, resp.StatusCode, repo, ref)
		}
		var m map[string]any
		return m, json.NewDecoder(resp.Body).Decode(&m)
	}
	manifest, err := get(tag)
	if err != nil || manifest == nil {
		return "", err
	}
	if list, ok := manifest["manifests"].([]any); ok { // an index: pick linux/amd64
		var digest string
		for _, entry := range list {
			e, _ := entry.(map[string]any)
			p, _ := e["platform"].(map[string]any)
			if p["os"] == "linux" && p["architecture"] == "amd64" {
				digest, _ = e["digest"].(string)
				break
			}
		}
		if digest == "" {
			return "", nil
		}
		if manifest, err = get(digest); err != nil || manifest == nil {
			return "", err
		}
	}
	cfg, _ := manifest["config"].(map[string]any)
	digest, _ := cfg["digest"].(string)
	return ShortID(digest), nil
}
