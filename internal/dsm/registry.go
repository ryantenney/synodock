package dsm

import (
	"sort"
	"strconv"
)

// Registry is a registry Container Manager pulls from. Passwords aren't listed back.
type Registry struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Syno     bool   `json:"syno"`
}

// RegistryList is the registries, by name, and the name of the one in use.
func (c *Client) RegistryList() ([]Registry, string, error) {
	var data struct {
		Registries []Registry `json:"registries"`
		Using      string     `json:"using"`
	}
	if err := c.CallInto(&data, "SYNO.Docker.Registry", "get", 1, Params{"limit": "-1", "offset": "0"}); err != nil {
		return nil, "", err
	}
	sort.Slice(data.Registries, func(i, j int) bool { return data.Registries[i].Name < data.Registries[j].Name })
	return data.Registries, data.Using, nil
}

// RegistryAdd adds a registry, with its account when it has one.
func (c *Client) RegistryAdd(name, url, username, password string) error {
	_, err := c.Call("SYNO.Docker.Registry", "create", 1, Params{
		"name": JS(name), "url": JS(url), "username": JS(username), "password": JS(password),
		"enable_trust_SSC": "false", "enable_registry_mirror": "false",
	})
	return err
}

// RegistryRemove deletes a registry by name.
func (c *Client) RegistryRemove(name string) error {
	_, err := c.Call("SYNO.Docker.Registry", "delete", 1, Params{"name": JS(name)})
	return err
}

// RegistryUse makes a registry the one Container Manager searches and pulls from by default.
func (c *Client) RegistryUse(name string) error {
	_, err := c.Call("SYNO.Docker.Registry", "using", 1, Params{"name": JS(name)})
	return err
}

// SearchResult is one repository a registry search found.
type SearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	IsOfficial  bool   `json:"is_official"`
}

// RegistrySearch searches the registry in use, which for Docker Hub is the default.
func (c *Client) RegistrySearch(query string, limit int) ([]SearchResult, error) {
	var data struct {
		Data []SearchResult `json:"data"`
	}
	err := c.CallInto(&data, "SYNO.Docker.Registry", "search", 1, Params{
		"offset": "0", "limit": strconv.Itoa(limit), "page_size": strconv.Itoa(limit), "q": JS(query)})
	return data.Data, err
}

// RegistryTags lists the tags of a repository in the registry in use.
func (c *Client) RegistryTags(repo string) ([]string, error) {
	var data []struct {
		Tag string `json:"tag"`
	}
	if err := c.CallInto(&data, "SYNO.Docker.Registry", "tags", 1, Params{"repo": JS(repo)}); err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(data))
	for _, t := range data {
		tags = append(tags, t.Tag)
	}
	return tags, nil
}
