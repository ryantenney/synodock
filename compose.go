package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// imageLine finds each `image:` in a compose file, as dsm.py does: a plain text match,
// since a compose file's variables aren't interpolated for this.
var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*['"]?([^'"\s#]+)`)

// composeImages are the image references a compose file runs, with Docker's implicit
// :latest added.
func composeImages(text string) []string {
	var out []string
	for _, m := range imageLine.FindAllStringSubmatch(text, -1) {
		ref := m[1]
		if last := ref[strings.LastIndex(ref, "/")+1:]; !strings.Contains(last, ":") {
			ref += ":latest"
		}
		out = append(out, ref)
	}
	return out
}

// composeContainerNames are the containers a compose file runs: container_name, else
// compose's <project>-<service>-1. Services that start only under a profile are left out,
// since a build doesn't start them.
func composeContainerNames(text, projectName string) ([]string, error) {
	var file struct {
		Services map[string]struct {
			ContainerName string   `yaml:"container_name"`
			Profiles      []string `yaml:"profiles"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(text), &file); err != nil {
		return nil, fmt.Errorf("compose file: %w", err)
	}
	var names []string
	for service, svc := range file.Services {
		if len(svc.Profiles) > 0 {
			continue
		}
		name := svc.ContainerName
		if name == "" {
			name = projectName + "-" + service + "-1"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
